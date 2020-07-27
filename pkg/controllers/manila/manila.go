package manila

import (
	"context"
	"reflect"
	"time"

	"github.com/gophercloud/gophercloud"
	"github.com/gophercloud/gophercloud/openstack/sharedfilesystems/v2/sharetypes"
	operatorv1 "github.com/openshift/api/operator/v1"
	"github.com/openshift/csi-driver-manila-operator/pkg/util"
	"github.com/openshift/library-go/pkg/controller/factory"
	"github.com/openshift/library-go/pkg/operator/events"
	"github.com/openshift/library-go/pkg/operator/resource/resourceapply"
	"github.com/openshift/library-go/pkg/operator/resource/resourcemerge"
	"github.com/openshift/library-go/pkg/operator/v1helpers"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/client-go/kubernetes"
	storagelisters "k8s.io/client-go/listers/storage/v1"
	"k8s.io/klog"
)

// This Controller watches OpenStack and:
// 1) Installs Manila CSI drivers (Manila itself, NFS) once
//    it detects that there is Manlina present (by running provided
//    manilaOperatorSet).
// 2) Creates StorageClass for each share type provided by Manila.
// 3) If there is no Manila in the OpenStack where the cluster runs,
//    it marks the operator with condition Disabled=true.
//
// Note that the CSI driver(s) are not un-installed when Manila becomes
// missing or it stops providing shares of given type - Manila bight be
// under (short?) maintenance / reconfiguration.
// Similarly, StorageClasses are not deleted when a share type disappears
// from Manila.
type Controller struct {
	operatorClient     v1helpers.OperatorClient
	kubeClient         kubernetes.Interface
	openStackClient    *openStackClient
	storageClassLister storagelisters.StorageClassLister
	// Controllers to start when Manila is detected
	csiControllers     []Runnable
	controllersRunning bool
	eventRecorder      events.Recorder
}

type Runnable interface {
	Run(ctx context.Context, workers int)
}

const (
	// Minimal interval between controller resyncs. The controller will detect
	// new share types in Manila and create StorageClasses for them at least
	// once per this interval.
	resyncInterval = 1 * time.Minute

	operatorConditionPrefix = "ManilaController"
)

func NewController(
	operatorClient v1helpers.OperatorClient,
	kubeClient kubernetes.Interface,
	informers v1helpers.KubeInformersForNamespaces,
	openStackClient *openStackClient,
	csiControllers []Runnable,
	eventRecorder events.Recorder) factory.Controller {

	scInformer := informers.InformersFor("").Storage().V1().StorageClasses()
	c := &Controller{
		operatorClient:     operatorClient,
		kubeClient:         kubeClient,
		storageClassLister: scInformer.Lister(),
		openStackClient:    openStackClient,
		csiControllers:     csiControllers,
		eventRecorder:      eventRecorder,
	}
	return factory.New().WithSync(c.sync).WithSyncDegradedOnError(operatorClient).ResyncEvery(resyncInterval).WithInformers(
		operatorClient.Informer(),
		scInformer.Informer(),
	).ToController("ManilaController", eventRecorder)
}

func (c *Controller) sync(ctx context.Context, syncCtx factory.SyncContext) error {
	klog.V(4).Infof("Manila sync started")
	defer klog.V(4).Infof("Manila sync finished")

	opSpec, _, _, err := c.operatorClient.GetOperatorState()
	if err != nil {
		return err
	}
	if opSpec.ManagementState != operatorv1.Managed {
		return nil
	}

	shareTypes, err := c.openStackClient.GetShareTypes()
	if err != nil {
		switch err.(type) {
		case *gophercloud.ErrEndpointNotFound:
			// OpenStack does not support manila, report the operator as disabled
			return c.setDisabled("This OpenStack does not provide Manila service")
		default:
			return err
		}
	}

	if len(shareTypes) == 0 {
		klog.V(4).Infof("Manila does not provide any share types")
		return c.setDisabled("Manila does not provide any share types")
	}
	// Manila has some shares: start the actual CSI driver controller sets
	if !c.controllersRunning {
		klog.V(4).Infof("Starting CSI driver controllers")
		for _, ctrl := range c.csiControllers {
			go ctrl.Run(ctx, 1)
		}
		c.controllersRunning = true
	}
	err = c.syncStorageClasses(ctx, shareTypes)
	if err != nil {
		return err
	}

	return c.setEnabled()
}

func (c *Controller) syncStorageClasses(ctx context.Context, shareTypes []sharetypes.ShareType) error {
	var errs []error
	for _, shareType := range shareTypes {
		klog.V(4).Infof("Syncing storage class for shareType type %s", shareType.Name)
		sc := c.generateStorageClass(shareType)
		err := c.applyStorageClass(ctx, sc)
		if err != nil {
			errs = append(errs, err)
		}
	}
	if len(errs) != 0 {
		return errors.NewAggregate(errs)
	}
	return nil
}

func (c *Controller) applyStorageClass(ctx context.Context, expected *storagev1.StorageClass) error {
	current, err := c.storageClassLister.Get(expected.Name)
	if err == nil {
		if !reflect.DeepEqual(expected.Parameters, current.Parameters) {
			// StorageClass.Parameters changed. Typically, secret namespace
			// is different when moving from OLM to non-OLM operator.
			// Delete the old class and create a new one.
			if err := c.kubeClient.StorageV1().StorageClasses().Delete(ctx, expected.Name, metav1.DeleteOptions{}); err != nil {
				if apierrors.IsNotFound(err) {
					err = nil
				}
				return err
			}
			// Merge existing and expected ObjectMeta (esp. default storage class)
			var modified bool
			currentCopy := current.DeepCopy()
			resourcemerge.EnsureObjectMeta(&modified, &currentCopy.ObjectMeta, expected.ObjectMeta)
			expected.ObjectMeta = currentCopy.ObjectMeta
			// Fall through to ApplyStorageClass, it will create a new class.
		}
	}
	_, _, err = resourceapply.ApplyStorageClass(c.kubeClient.StorageV1(), c.eventRecorder, expected)
	return err
}

func (c *Controller) generateStorageClass(shareType sharetypes.ShareType) *storagev1.StorageClass {
	storageClassName := util.StorageClassNamePrefix + shareType.Name
	sc := &storagev1.StorageClass{
		ObjectMeta: metav1.ObjectMeta{
			Name: storageClassName,
		},
		Provisioner: "manila.csi.openstack.org",
		Parameters: map[string]string{
			"type": shareType.Name,
			"csi.storage.k8s.io/provisioner-secret-name":       util.ManilaSecretName,
			"csi.storage.k8s.io/provisioner-secret-namespace":  util.OperatorNamespace,
			"csi.storage.k8s.io/node-stage-secret-name":        util.ManilaSecretName,
			"csi.storage.k8s.io/node-stage-secret-namespace":   util.OperatorNamespace,
			"csi.storage.k8s.io/node-publish-secret-name":      util.ManilaSecretName,
			"csi.storage.k8s.io/node-publish-secret-namespace": util.OperatorNamespace,
		},
	}
	return sc
}

func (c *Controller) setEnabled() error {
	availableCnd := operatorv1.OperatorCondition{
		Type:   operatorConditionPrefix + operatorv1.OperatorStatusTypeAvailable,
		Status: operatorv1.ConditionTrue,
	}
	_, _, err := v1helpers.UpdateStatus(c.operatorClient,
		v1helpers.UpdateConditionFn(availableCnd),
		removeConditionFn(operatorConditionPrefix+"Disabled"))
	return err
}

func (c *Controller) setDisabled(msg string) error {
	disabledCnd := operatorv1.OperatorCondition{
		Type:    operatorConditionPrefix + "Disabled",
		Status:  operatorv1.ConditionTrue,
		Reason:  "NoManila",
		Message: msg,
	}
	_, _, err := v1helpers.UpdateStatus(c.operatorClient,
		v1helpers.UpdateConditionFn(disabledCnd),
		removeConditionFn(operatorConditionPrefix+operatorv1.OperatorStatusTypeAvailable))
	return err
}

func removeConditionFn(cnd string) v1helpers.UpdateStatusFunc {
	return func(oldStatus *operatorv1.OperatorStatus) error {
		v1helpers.RemoveOperatorCondition(&oldStatus.Conditions, cnd)
		return nil
	}
}
