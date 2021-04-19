package secrets

import (
	"context"
	"fmt"
	context2 "github.com/loft-sh/vcluster/cmd/vcluster/context"
	"github.com/loft-sh/vcluster/pkg/constants"
	"github.com/loft-sh/vcluster/pkg/controllers/resources/generic"
	"github.com/loft-sh/vcluster/pkg/controllers/resources/ingresses"
	"github.com/loft-sh/vcluster/pkg/controllers/resources/pods"
	"github.com/loft-sh/vcluster/pkg/util/clienthelper"
	"github.com/loft-sh/vcluster/pkg/util/loghelper"
	"github.com/loft-sh/vcluster/pkg/util/translate"
	"github.com/pkg/errors"
	corev1 "k8s.io/api/core/v1"
	networkingv1beta1 "k8s.io/api/networking/v1beta1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	v1core "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"
	"strings"
	"time"
)

func indexPodBySecret(rawObj client.Object) []string {
	pod := rawObj.(*corev1.Pod)
	return pods.SecretNamesFromPod(pod)
}

func Register(ctx *context2.ControllerContext) error {
	// index pods by their used secrets
	err := ctx.VirtualManager.GetFieldIndexer().IndexField(ctx.Context, &corev1.Pod{}, constants.IndexBySecret, indexPodBySecret)
	if err != nil {
		return err
	}

	includeIngresses := strings.Contains(ctx.Options.DisableSyncResources, "ingresses") == false
	if includeIngresses {
		err := ctx.VirtualManager.GetFieldIndexer().IndexField(ctx.Context, &networkingv1beta1.Ingress{}, constants.IndexBySecret, func(rawObj client.Object) []string {
			ingress := rawObj.(*networkingv1beta1.Ingress)
			return ingresses.SecretNamesFromIngress(ingress)
		})
		if err != nil {
			return err
		}
	}

	eventBroadcaster := record.NewBroadcaster()
	eventBroadcaster.StartRecordingToSink(&v1core.EventSinkImpl{Interface: kubernetes.NewForConfigOrDie(ctx.VirtualManager.GetConfig()).CoreV1().Events("")})
	return generic.RegisterSyncer(ctx, &syncer{
		eventRecoder:    eventBroadcaster.NewRecorder(ctx.VirtualManager.GetScheme(), corev1.EventSource{Component: "secret-syncer"}),
		targetNamespace: ctx.Options.TargetNamespace,
		virtualClient:   ctx.VirtualManager.GetClient(),
		localClient:     ctx.LocalManager.GetClient(),

		includeIngresses: includeIngresses,
	}, "secret", generic.RegisterSyncerOptions{
		ModifyForwardSyncer: func(builder *builder.Builder) *builder.Builder {
			if includeIngresses {
				builder = builder.Watches(&source.Kind{Type: &networkingv1beta1.Ingress{}}, handler.EnqueueRequestsFromMapFunc(mapIngresses))
			}

			return builder.Watches(&source.Kind{Type: &corev1.Pod{}}, handler.EnqueueRequestsFromMapFunc(mapPods))
		},
	})
}

func mapIngresses(obj client.Object) []reconcile.Request {
	ingress, ok := obj.(*networkingv1beta1.Ingress)
	if !ok {
		return nil
	}

	requests := []reconcile.Request{}
	names := ingresses.SecretNamesFromIngress(ingress)
	for _, name := range names {
		splitted := strings.Split(name, "/")
		if len(splitted) == 2 {
			requests = append(requests, reconcile.Request{
				NamespacedName: types.NamespacedName{
					Namespace: splitted[0],
					Name:      splitted[1],
				},
			})
		}
	}

	return requests
}

func mapPods(obj client.Object) []reconcile.Request {
	pod, ok := obj.(*corev1.Pod)
	if !ok {
		return nil
	}

	requests := []reconcile.Request{}
	names := pods.SecretNamesFromPod(pod)
	for _, name := range names {
		splitted := strings.Split(name, "/")
		if len(splitted) == 2 {
			requests = append(requests, reconcile.Request{
				NamespacedName: types.NamespacedName{
					Namespace: splitted[0],
					Name:      splitted[1],
				},
			})
		}
	}

	return requests
}

type syncer struct {
	eventRecoder    record.EventRecorder
	targetNamespace string

	virtualClient client.Client
	localClient   client.Client

	includeIngresses bool
}

func (s *syncer) New() client.Object {
	return &corev1.Secret{}
}

func (s *syncer) NewList() client.ObjectList {
	return &corev1.SecretList{}
}

func (s *syncer) translate(vObj client.Object) (*corev1.Secret, error) {
	newObj, err := translate.SetupMetadata(s.targetNamespace, vObj)
	if err != nil {
		return nil, errors.Wrap(err, "error setting metadata")
	}

	newSecret := newObj.(*corev1.Secret)
	if newSecret.Type == corev1.SecretTypeServiceAccountToken {
		newSecret.Type = corev1.SecretTypeOpaque
	}

	return newSecret, nil
}

func (s *syncer) ForwardCreate(ctx context.Context, vObj client.Object, log loghelper.Logger) (ctrl.Result, error) {
	createNeeded, err := s.ForwardCreateNeeded(vObj)
	if err != nil {
		return ctrl.Result{}, err
	} else if createNeeded == false {
		return ctrl.Result{}, nil
	}

	vSecret := vObj.(*corev1.Secret)
	newSecret, err := s.translate(vObj)
	if err != nil {
		return ctrl.Result{}, err
	}

	err = clienthelper.Apply(ctx, s.localClient, newSecret, log)
	if err != nil {
		log.Infof("error syncing %s/%s to physical cluster: %v", vSecret.Namespace, vSecret.Name, err)
		s.eventRecoder.Eventf(vSecret, "Warning", "SyncError", "Error syncing to physical cluster: %v", err)
		return ctrl.Result{RequeueAfter: time.Second}, err
	}

	return ctrl.Result{}, nil
}

func (s *syncer) ForwardCreateNeeded(vObj client.Object) (bool, error) {
	return s.isSecretUsed(vObj)
}

func (s *syncer) isSecretUsed(vObj runtime.Object) (bool, error) {
	secret, ok := vObj.(*corev1.Secret)
	if !ok || secret == nil {
		return false, fmt.Errorf("%#v is not a secret", vObj)
	}

	podList := &corev1.PodList{}
	err := s.virtualClient.List(context.TODO(), podList, client.MatchingFields{constants.IndexBySecret: secret.Namespace + "/" + secret.Name})
	if err != nil {
		return false, err
	}

	if len(podList.Items) > 0 {
		return true, nil
	}

	// check if we also sync ingresses
	if s.includeIngresses {
		ingressesList := &networkingv1beta1.IngressList{}
		err := s.virtualClient.List(context.TODO(), ingressesList, client.MatchingFields{constants.IndexBySecret: secret.Namespace + "/" + secret.Name})
		if err != nil {
			return false, err
		}

		return len(ingressesList.Items) > 0, nil
	}

	return false, nil
}

func (s *syncer) ForwardUpdate(ctx context.Context, pObj client.Object, vObj client.Object, log loghelper.Logger) (ctrl.Result, error) {
	used, err := s.isSecretUsed(vObj)
	if err != nil {
		return ctrl.Result{}, err
	} else if used == false {
		pSecret, _ := meta.Accessor(pObj)
		log.Debugf("delete physical secret %s/%s, because it is not used anymore", pSecret.GetNamespace(), pSecret.GetName())
		err = s.localClient.Delete(ctx, pObj)
		if err != nil {
			log.Infof("error deleting physical object %s/%s in physical cluster: %v", pSecret.GetNamespace(), pSecret.GetName(), err)
			return ctrl.Result{}, err
		}

		return ctrl.Result{}, nil
	}

	return s.ForwardCreate(ctx, vObj, log)
}

func (s *syncer) ForwardUpdateNeeded(pObj client.Object, vObj client.Object) (bool, error) {
	used, err := s.isSecretUsed(vObj)
	if err != nil {
		return false, err
	} else if used == false {
		return true, nil
	}

	newSecret, err := s.translate(vObj)
	if err != nil {
		return false, err
	}

	equal, err := clienthelper.AppliedObjectsEqual(pObj, newSecret)
	if err != nil {
		return false, err
	}

	return equal == false, nil
}

func (s *syncer) BackwardUpdate(ctx context.Context, pObj client.Object, vObj client.Object, log loghelper.Logger) (ctrl.Result, error) {
	return ctrl.Result{}, nil
}

func (s *syncer) BackwardUpdateNeeded(pObj client.Object, vObj client.Object) (bool, error) {
	return false, nil
}