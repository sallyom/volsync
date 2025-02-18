/*
Copyright 2020 The VolSync authors.

This program is free software: you can redistribute it and/or modify
it under the terms of the GNU Affero General Public License as published
by the Free Software Foundation, either version 3 of the License, or
(at your option) any later version.

This program is distributed in the hope that it will be useful,
but WITHOUT ANY WARRANTY; without even the implied warranty of
MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
GNU Affero General Public License for more details.

You should have received a copy of the GNU Affero General Public License
along with this program.  If not, see <https://www.gnu.org/licenses/>.
*/

package controllers

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	snapv1 "github.com/kubernetes-csi/external-snapshotter/client/v4/apis/volumesnapshot/v1beta1"
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	gomegatypes "github.com/onsi/gomega/types"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/controller-runtime/pkg/envtest/printer"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	volsyncv1alpha1 "github.com/backube/volsync/api/v1alpha1"
	"github.com/backube/volsync/controllers/mover/rclone"
	"github.com/backube/volsync/controllers/mover/restic"
	"github.com/backube/volsync/controllers/mover/rsync"
	//+kubebuilder:scaffold:imports
)

const (
	duration       = 10 * time.Second
	maxWait        = 60 * time.Second
	interval       = 250 * time.Millisecond
	dataVolumeName = "data"
)

// These tests use Ginkgo (BDD-style Go testing framework). Refer to
// http://onsi.github.io/ginkgo/ to learn more about Ginkgo.

var k8sClient client.Client
var testEnv *envtest.Environment
var cancel context.CancelFunc

func TestAPIs(t *testing.T) {
	RegisterFailHandler(Fail)

	RunSpecsWithDefaultAndCustomReporters(t,
		"Controller",
		[]Reporter{printer.NewlineReporter{}})
}

var _ = BeforeSuite(func() {
	logf.SetLogger(zap.New(zap.WriteTo(GinkgoWriter), zap.UseDevMode(true)))

	var ctx context.Context
	ctx, cancel = context.WithCancel(context.TODO())

	By("bootstrapping test environment")
	testEnv = &envtest.Environment{
		CRDDirectoryPaths: []string{
			// VolSync CRDs
			filepath.Join("..", "config", "crd", "bases"),
			// Snapshot CRDs
			filepath.Join("..", "hack", "crds"),
		},
		ErrorIfCRDPathMissing: true,
	}

	cfg, err := testEnv.Start()
	Expect(err).ToNot(HaveOccurred())
	Expect(cfg).ToNot(BeNil())

	err = volsyncv1alpha1.AddToScheme(scheme.Scheme)
	Expect(err).NotTo(HaveOccurred())

	err = snapv1.AddToScheme(scheme.Scheme)
	Expect(err).NotTo(HaveOccurred())

	//+kubebuilder:scaffold:scheme

	/*
		// From original boilerplate
		k8sClient, err = client.New(cfg, client.Options{Scheme: scheme.Scheme})
		Expect(err).ToNot(HaveOccurred())
		Expect(k8sClient).ToNot(BeNil())
	*/

	// Register the data movers
	rsync.Register()
	rclone.Register()
	restic.Register()

	k8sManager, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme:             scheme.Scheme,
		MetricsBindAddress: "0",
	})
	Expect(err).ToNot(HaveOccurred())

	err = (&ReplicationDestinationReconciler{
		Client: k8sManager.GetClient(),
		Log:    ctrl.Log.WithName("controllers").WithName("Destination"),
		Scheme: k8sManager.GetScheme(),
	}).SetupWithManager(k8sManager)
	Expect(err).ToNot(HaveOccurred())

	err = (&ReplicationSourceReconciler{
		Client: k8sManager.GetClient(),
		Log:    ctrl.Log.WithName("controllers").WithName("Source"),
		Scheme: k8sManager.GetScheme(),
	}).SetupWithManager(k8sManager)
	Expect(err).ToNot(HaveOccurred())

	go func() {
		defer GinkgoRecover()
		err = k8sManager.Start(ctx)
		Expect(err).ToNot(HaveOccurred())
	}()

	k8sClient = k8sManager.GetClient()
	Expect(k8sClient).ToNot(BeNil())
}, 60)

var _ = AfterSuite(func() {
	cancel()
	By("tearing down the test environment")
	err := testEnv.Stop()
	Expect(err).ToNot(HaveOccurred())
})

// beOwnedBy is a GomegaMatcher that ensures a Kubernetes Object is owned by a
// specific other object.
func beOwnedBy(owner interface{}) gomegatypes.GomegaMatcher {
	return &ownerRefMatcher{
		owner: owner,
	}
}

type ownerRefMatcher struct {
	owner  interface{}
	reason string
}

func (m *ownerRefMatcher) Match(actual interface{}) (success bool, err error) {
	actObj, ok := actual.(metav1.Object)
	if !ok {
		return false, fmt.Errorf("actual value is not a metav1.Object")
	}
	ownerObj, ok := m.owner.(metav1.Object)
	if !ok {
		return false, fmt.Errorf("expected value is not a metav1.Object")
	}
	controller := metav1.GetControllerOf(actObj)
	if controller == nil {
		m.reason = "it does not have an owner"
		return false, nil
	}
	if controller.UID != ownerObj.GetUID() {
		m.reason = "it does not refer to the expected parent object"
		return false, nil
	}
	// XXX: This check isn't perfect. Both cluster-scoped and objects in the
	// "default" namespace have an empty namespace name. So the following may
	// (incorrectly) pass for namespaced owners in the default namespace
	// attempting to own cluster-scoped objects.
	if ownerObj.GetNamespace() != "" { // if owner not cluster-scoped
		if actObj.GetNamespace() != ownerObj.GetNamespace() {
			m.reason = "cross namespace owner references are not allowed"
			return false, nil
		}
	}
	return true, nil
}
func (m *ownerRefMatcher) FailureMessage(actual interface{}) (message string) {
	return fmt.Sprintf("Expected\n\t%#v\nto be owned by\n\t%#v\nbut %v", actual, m.owner, m.reason)
}
func (m *ownerRefMatcher) NegatedFailureMessage(actual interface{}) (message string) {
	return fmt.Sprintf("Expected\n\t%#v\nnot to be owned by\n\t%#v", actual, m.owner)
}
