/*
Copyright 2017 Caicloud authors. All rights reserved.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package ipvsdr

import (
	"fmt"
	"math/rand"
	"reflect"
	"strings"
	"time"

	log "github.com/zoumo/logdog"

	"github.com/caicloud/loadbalancer-controller/config"
	netv1alpha1 "github.com/caicloud/loadbalancer-controller/pkg/apis/networking/v1alpha1"
	"github.com/caicloud/loadbalancer-controller/pkg/informers"
	netlisters "github.com/caicloud/loadbalancer-controller/pkg/listers/networking/v1alpha1"
	"github.com/caicloud/loadbalancer-controller/pkg/toleration"
	"github.com/caicloud/loadbalancer-controller/pkg/tprclient"
	controllerutil "github.com/caicloud/loadbalancer-controller/pkg/util/controller"
	lbutil "github.com/caicloud/loadbalancer-controller/pkg/util/lb"
	"github.com/caicloud/loadbalancer-controller/pkg/util/validation"
	"github.com/caicloud/loadbalancer-controller/provider"

	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/kubernetes"
	corelisters "k8s.io/client-go/listers/core/v1"
	extensionslisters "k8s.io/client-go/listers/extensions/v1beta1"
	"k8s.io/client-go/pkg/api/v1"
	extensions "k8s.io/client-go/pkg/apis/extensions/v1beta1"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/kubernetes/pkg/controller"
)

const (
	providerNameSuffix = "-provider-ipvsdr"
	providerName       = "ipvsdr"
)

// controllerKind contains the schema.GroupVersionKind for this controller type.
var controllerKind = netv1alpha1.SchemeGroupVersion.WithKind(netv1alpha1.LoadBalancerKind)

func init() {
	provider.RegisterPlugin(providerName, NewIpvsdr())
}

var _ provider.Plugin = &ipvsdr{}

type ipvsdr struct {
	initialized bool

	image string

	client    kubernetes.Interface
	tprclient tprclient.Interface

	helper *controllerutil.Helper

	lbLister  netlisters.LoadBalancerLister
	dLister   extensionslisters.DeploymentLister
	podLister corelisters.PodLister

	queue workqueue.RateLimitingInterface
}

// NewIpvsdr creates a new ipvsdr provider plugin
func NewIpvsdr() provider.Plugin {
	return &ipvsdr{}
}

func (f *ipvsdr) Init(cfg config.Configuration, sif informers.SharedInformerFactory) {
	if f.initialized {
		return
	}
	f.initialized = true

	log.Info("Initialize the ipvsdr provider")

	// set config
	f.image = cfg.Providers.Ipvsdr.Image
	f.client = cfg.Client
	f.tprclient = cfg.TPRClient

	// initialize controller
	lbInformer := sif.Networking().V1alpha1().LoadBalancer()
	dInformer := sif.Extensions().V1beta1().Deployments()
	podInfomer := sif.Core().V1().Pods()

	f.lbLister = lbInformer.Lister()
	f.dLister = dInformer.Lister()
	f.podLister = podInfomer.Lister()

	f.queue = workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "provider-ipvsdr")
	f.helper = controllerutil.NewHelperForKeyFunc(&netv1alpha1.LoadBalancer{}, f.queue, f.syncLoadBalancer, controllerutil.PassthroughKeyFunc)

	dInformer.Informer().AddEventHandler(lbutil.NewEventHandlerForDeployment(f.lbLister, f.dLister, f.helper, f.deploymentFiltered))
	podInfomer.Informer().AddEventHandler(lbutil.NewEventHandlerForSyncStatusWithPod(f.lbLister, f.podLister, f.helper, f.podFiltered))
}

func (f *ipvsdr) Run(stopCh <-chan struct{}) {

	workers := 1

	if !f.initialized {
		log.Panic("Please initialize provider before you run it")
		return
	}

	defer utilruntime.HandleCrash()

	log.Info("Starting ipvsdr provider", log.Fields{"workers": workers, "image": f.image})
	defer log.Info("Shutting down ipvsdr provider")

	// lb controller has waited all the informer synced
	// there is no need to wait again here

	defer func() {
		log.Info("Shutting down ipvsdr provider")
		f.helper.ShutDown()
	}()

	f.helper.Run(workers, stopCh)

	<-stopCh
}

func (f *ipvsdr) selector(lb *netv1alpha1.LoadBalancer) labels.Set {
	return labels.Set{
		netv1alpha1.LabelKeyCreatedBy: fmt.Sprintf(netv1alpha1.LabelValueFormatCreateby, lb.Namespace, lb.Name),
		netv1alpha1.LabelKeyProvider:  providerName,
	}
}

// filter Deployment that controller does not care
func (f *ipvsdr) deploymentFiltered(obj *extensions.Deployment) bool {
	return f.filteredByLabel(obj)
}

func (f *ipvsdr) podFiltered(obj *v1.Pod) bool {
	return f.filteredByLabel(obj)
}

func (f *ipvsdr) filteredByLabel(obj metav1.ObjectMetaAccessor) bool {
	// obj.Labels
	selector := labels.Set{netv1alpha1.LabelKeyProvider: providerName}.AsSelector()
	match := selector.Matches(labels.Set(obj.GetObjectMeta().GetLabels()))

	return !match
}

func (f *ipvsdr) OnSync(lb *netv1alpha1.LoadBalancer) {
	if lb.Spec.Type != netv1alpha1.LoadBalancerTypeExternal && lb.Spec.Providers.Ipvsdr != nil {
		// It is not my responsible
		return
	}
	log.Info("Syncing providers, triggered by lb controller", log.Fields{"lb": lb.Name, "namespace": lb.Namespace})
	f.helper.Enqueue(lb)
}

func (f *ipvsdr) syncLoadBalancer(obj interface{}) error {
	lb, ok := obj.(*netv1alpha1.LoadBalancer)
	if !ok {
		return fmt.Errorf("expect loadbalancer, got %v", obj)
	}

	// Validate loadbalancer scheme
	if err := validation.ValidateLoadBalancer(lb); err != nil {
		log.Debug("invalid loadbalancer scheme", log.Fields{"err": err})
		return err
	}

	key, _ := controllerutil.KeyFunc(lb)

	startTime := time.Now()
	defer func() {
		log.Debug("Finished syncing ipvsdr provider", log.Fields{"lb": key, "usedTime": time.Since(startTime)})
	}()

	nlb, err := f.lbLister.LoadBalancers(lb.Namespace).Get(lb.Name)
	if errors.IsNotFound(err) {
		log.Warn("LoadBalancer has been deleted, clean up provider", log.Fields{"lb": key})

		return f.cleanup(lb)
	}
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("Unable to retrieve LoadBalancer %v from store: %v", key, err))
		return err
	}

	// fresh lb
	if lb.UID != nlb.UID {
		return nil
	}
	lb = nlb

	ds, err := f.getDeploymentsForLoadBalancer(lb)
	if err != nil {
		return err
	}

	if lb.DeletionTimestamp != nil {
		// TODO sync status only
		return nil
	}

	return f.sync(lb, ds)
}

func (f *ipvsdr) getDeploymentsForLoadBalancer(lb *netv1alpha1.LoadBalancer) ([]*extensions.Deployment, error) {

	// construct selector
	selector := f.selector(lb).AsSelector()

	// list all
	dList, err := f.dLister.Deployments(lb.Namespace).List(selector)
	if err != nil {
		return nil, err
	}

	// If any adoptions are attempted, we should first recheck for deletion with
	// an uncached quorum read sometime after listing deployment (see kubernetes#42639).
	canAdoptFunc := controller.RecheckDeletionTimestamp(func() (metav1.Object, error) {
		// fresh lb
		fresh, err := f.tprclient.NetworkingV1alpha1().LoadBalancers(lb.Namespace).Get(lb.Name, metav1.GetOptions{})
		if err != nil {
			return nil, err
		}

		if fresh.UID != lb.UID {
			return nil, fmt.Errorf("original LoadBalancer %v/%v is gone: got uid %v, wanted %v", lb.Namespace, lb.Name, fresh.UID, lb.UID)
		}
		return fresh, nil
	})

	cm := controllerutil.NewDeploymentControllerRefManager(f.client, lb, selector, controllerKind, canAdoptFunc)
	return cm.Claim(dList)
}

// sync generate desired deployment from lb and compare it with existing deployment
func (f *ipvsdr) sync(lb *netv1alpha1.LoadBalancer, dps []*extensions.Deployment) error {
	desiredDeploy := f.generateDeployment(lb)

	// update
	updated := false
	activeDeploy := desiredDeploy

	for _, dp := range dps {
		// two conditions will trigger controller to scale down deployment
		// 1. deployment does not have auto-generated prefix
		// 2. if there are more than one active controllers, there may be many valid deployments.
		//    But we only need one.
		if !strings.HasPrefix(dp.Name, lb.Name+providerNameSuffix) || updated {
			if *dp.Spec.Replicas == 0 {
				continue
			}
			// scale unexpected deployment replicas to zero
			log.Info("Scale unexpected provider replicas to zero", log.Fields{"d.name": dp.Name, "lb.name": lb.Name})
			copy, _ := lbutil.DeploymentDeepCopy(dp)
			replica := int32(0)
			copy.Spec.Replicas = &replica
			f.client.ExtensionsV1beta1().Deployments(lb.Namespace).Update(copy)
			continue
		}

		updated = true
		copyDp, changed, err := f.ensureDeployment(desiredDeploy, dp)
		if err != nil {
			continue
		}
		if changed {
			log.Info("Sync ipvsdr for lb", log.Fields{"d.name": dp.Name, "lb.name": lb.Name})
			_, err = f.client.ExtensionsV1beta1().Deployments(lb.Namespace).Update(copyDp)
			if err != nil {
				return err
			}
		}

		activeDeploy = copyDp
	}

	// len(dps) == 0 or no deployment's name match desired deployment
	if !updated {
		// create deployment
		log.Info("Create ipvsdr for lb", log.Fields{"d.name": desiredDeploy.Name, "lb.name": lb.Name})
		_, err := f.client.ExtensionsV1beta1().Deployments(lb.Namespace).Create(desiredDeploy)
		if err != nil {
			return err
		}
	}

	return f.syncStatus(lb, activeDeploy)
}

func (f *ipvsdr) ensureDeployment(desiredDeploy, oldDeploy *extensions.Deployment) (*extensions.Deployment, bool, error) {
	copyDp, err := lbutil.DeploymentDeepCopy(oldDeploy)
	if err != nil {
		return nil, false, err
	}

	// ensure labels
	for k, v := range desiredDeploy.Labels {
		copyDp.Labels[k] = v
	}
	// ensure replicas
	copyDp.Spec.Replicas = desiredDeploy.Spec.Replicas
	// ensure image
	copyDp.Spec.Template.Spec.Containers[0].Image = desiredDeploy.Spec.Template.Spec.Containers[0].Image
	// ensure nodeaffinity
	copyDp.Spec.Template.Spec.Affinity.NodeAffinity = desiredDeploy.Spec.Template.Spec.Affinity.NodeAffinity

	// check if changed
	nodeAffinityChanged := !reflect.DeepEqual(copyDp.Spec.Template.Spec.Affinity.NodeAffinity, oldDeploy.Spec.Template.Spec.Affinity.NodeAffinity)
	imageChanged := copyDp.Spec.Template.Spec.Containers[0].Image != oldDeploy.Spec.Template.Spec.Containers[0].Image
	labelChanged := !reflect.DeepEqual(copyDp.Labels, oldDeploy.Labels)
	replicasChanged := *(copyDp.Spec.Replicas) != *(oldDeploy.Spec.Replicas)

	changed := labelChanged || replicasChanged || nodeAffinityChanged || imageChanged
	if changed {
		log.Info("Abount to correct ipvsdr provider", log.Fields{
			"dp.name":             copyDp.Name,
			"labelChanged":        labelChanged,
			"replicasChanged":     replicasChanged,
			"nodeAffinityChanged": nodeAffinityChanged,
			"imageChanged":        imageChanged,
		})
	}

	return copyDp, changed, nil
}

// cleanup deployment and other resource controlled by ipvsdr provider
func (f *ipvsdr) cleanup(lb *netv1alpha1.LoadBalancer) error {

	ds, err := f.getDeploymentsForLoadBalancer(lb)
	if err != nil {
		return err
	}

	policy := metav1.DeletePropagationForeground
	gracePeriodSeconds := int64(30)
	for _, d := range ds {
		f.client.ExtensionsV1beta1().Deployments(d.Namespace).Delete(d.Name, &metav1.DeleteOptions{
			GracePeriodSeconds: &gracePeriodSeconds,
			PropagationPolicy:  &policy,
		})
	}

	return nil
}

func (f *ipvsdr) generateDeployment(lb *netv1alpha1.LoadBalancer) *extensions.Deployment {
	terminationGracePeriodSeconds := int64(30)
	hostNetwork := true
	replicas, _ := lbutil.CalculateReplicas(lb)
	privileged := true

	labels := f.selector(lb)

	// run in this node
	nodeAffinity := &v1.NodeAffinity{
		RequiredDuringSchedulingIgnoredDuringExecution: &v1.NodeSelector{
			NodeSelectorTerms: []v1.NodeSelectorTerm{
				{
					MatchExpressions: []v1.NodeSelectorRequirement{
						{
							Key:      fmt.Sprintf(netv1alpha1.UniqueLabelKeyFormat, lb.Namespace, lb.Name),
							Operator: v1.NodeSelectorOpIn,
							Values:   []string{"true"},
						},
					},
				},
			},
		},
	}

	// do not run with this pod
	podAffinity := &v1.PodAntiAffinity{
		RequiredDuringSchedulingIgnoredDuringExecution: []v1.PodAffinityTerm{
			{
				LabelSelector: &metav1.LabelSelector{
					MatchLabels: map[string]string{
						netv1alpha1.LabelKeyProvider: providerName,
					},
				},
				TopologyKey: metav1.LabelHostname,
			},
		},
	}

	t := true

	deploy := &extensions.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:   lb.Name + providerNameSuffix + "-" + lbutil.RandStringBytesRmndr(5),
			Labels: labels,
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion:         controllerKind.GroupVersion().String(),
					Kind:               controllerKind.Kind,
					Name:               lb.Name,
					UID:                lb.UID,
					Controller:         &t,
					BlockOwnerDeletion: &t,
				},
			},
		},
		Spec: extensions.DeploymentSpec{
			Replicas: &replicas,
			Template: v1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: labels,
				},
				Spec: v1.PodSpec{
					// host network ?
					HostNetwork: hostNetwork,
					// TODO
					TerminationGracePeriodSeconds: &terminationGracePeriodSeconds,
					Affinity: &v1.Affinity{
						// decide running on which node
						NodeAffinity: nodeAffinity,
						// don't co-locate pods of this deployment in same node
						PodAntiAffinity: podAffinity,
					},
					// tolerate taints
					Tolerations: toleration.GenerateTolerations(),
					Containers: []v1.Container{
						{
							Name:            providerName,
							Image:           f.image,
							ImagePullPolicy: v1.PullAlways,
							Resources: v1.ResourceRequirements{
								Limits: v1.ResourceList{
									v1.ResourceCPU:    resource.MustParse("200m"),
									v1.ResourceMemory: resource.MustParse("50Mi"),
								},
							},
							SecurityContext: &v1.SecurityContext{
								Privileged: &privileged,
							},
							Env: []v1.EnvVar{
								{
									Name: "POD_NAME",
									ValueFrom: &v1.EnvVarSource{
										FieldRef: &v1.ObjectFieldSelector{
											FieldPath: "metadata.name",
										},
									},
								},
								{
									Name: "POD_NAMESPACE",
									ValueFrom: &v1.EnvVarSource{
										FieldRef: &v1.ObjectFieldSelector{
											FieldPath: "metadata.namespace",
										},
									},
								},
								{
									Name:  "LOADBALANCER_NAMESPACE",
									Value: lb.Namespace,
								},
								{
									Name:  "LOADBALANCER_NAME",
									Value: lb.Name,
								},
							},
							VolumeMounts: []v1.VolumeMount{
								{
									Name:      "modules",
									MountPath: "/lib/modules",
									ReadOnly:  true,
								},
							},
						},
					},
					Volumes: []v1.Volume{
						{
							Name: "modules",
							VolumeSource: v1.VolumeSource{
								HostPath: &v1.HostPathVolumeSource{
									Path: "/lib/modules",
								},
							},
						},
					},
				},
			},
		},
	}

	return deploy
}

func (f *ipvsdr) getValidVRID() int {
	return rand.Intn(254) + 1
}
