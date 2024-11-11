/*
Copyright 2020 The Kubernetes Authors.

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

package k3s

import (
	"context"
	"errors"
	"fmt"

	corev1 "k8s.io/api/core/v1"
    appsv1 "k8s.io/api/apps/v1"
    gatewayv1alpha2 "sigs.k8s.io/gateway-api/apis/v1alpha2"
    gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
    resource "k8s.io/apimachinery/pkg/api/resource"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	kerrors "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/apiserver/pkg/storage/names"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	"sigs.k8s.io/cluster-api/controllers/external"
	"sigs.k8s.io/cluster-api/util/collections"
	"sigs.k8s.io/cluster-api/util/failuredomains"
	"sigs.k8s.io/cluster-api/util/patch"
	"sigs.k8s.io/controller-runtime/pkg/client"
    "k8s.io/apimachinery/pkg/util/intstr"

    

	bootstrapv1 "github.com/k3s-io/cluster-api-k3s/bootstrap/api/v1beta2"
	controlplanev1 "github.com/k3s-io/cluster-api-k3s/controlplane/api/v1beta2"
	"github.com/k3s-io/cluster-api-k3s/pkg/machinefilters"

	rest "k8s.io/client-go/rest"
    clientset "sigs.k8s.io/gateway-api/pkg/client/clientset/versioned"
    "strings"
)

var (
	ErrFailedToPickForDeletion   = errors.New("failed to pick machine to mark for deletion")
	ErrFailedToCreatePatchHelper = errors.New("failed to create patch for machine")
)

// ControlPlane holds business logic around control planes.
// It should never need to connect to a service, that responsibility lies outside of this struct.
// Going forward we should be trying to add more logic to here and reduce the amount of logic in the reconciler.
type ControlPlane struct {
	KCP                  *controlplanev1.KThreesControlPlane
	Cluster              *clusterv1.Cluster
	Machines             collections.Machines
	machinesPatchHelpers map[string]*patch.Helper

    // IsAgentless is true if the control plane is agentless.
    IsAgentless          bool

	// check if mgmt cluster has target cluster's etcd ca.
	// for old cluster created before connect-etcd feature, mgmt cluster don't
	// store etcd ca, controlplane reconcile loop need bypass any etcd operations.
	hasEtcdCA bool

	// reconciliationTime is the time of the current reconciliation, and should be used for all "now" calculations
	reconciliationTime metav1.Time

	// TODO: we should see if we can combine these with the Machine objects so we don't have all these separate lookups
	// See discussion on https://github.com/kubernetes-sigs/cluster-api/pull/3405
	KthreesConfigs map[string]*bootstrapv1.KThreesConfig
	InfraResources map[string]*unstructured.Unstructured
}

type ControlPlaneAgentlessDeployment struct {
    Deployment appsv1.Deployment
    Service corev1.Service
    TLSRoute gatewayv1alpha2.TLSRoute
}

// NewControlPlane returns an instantiated ControlPlane.
func NewControlPlane(ctx context.Context, client client.Client, cluster *clusterv1.Cluster, kcp *controlplanev1.KThreesControlPlane, ownedMachines collections.Machines) (*ControlPlane, error) {
	infraObjects, err := getInfraResources(ctx, client, ownedMachines)
	if err != nil {
		return nil, err
	}
	kthreesConfigs, err := getKThreesConfigs(ctx, client, ownedMachines)
	if err != nil {
		return nil, err
	}
	patchHelpers := map[string]*patch.Helper{}
	for _, machine := range ownedMachines {
		patchHelper, err := patch.NewHelper(machine, client)
		if err != nil {
			return nil, fmt.Errorf("machine: %s, %w", machine.Name, ErrFailedToCreatePatchHelper)
		}
		patchHelpers[machine.Name] = patchHelper
	}

	hasEtcdCA := false
	etcdCASecret := &corev1.Secret{}
	etcdCAObjectKey := types.NamespacedName{
		Namespace: cluster.Namespace,
		Name:      fmt.Sprintf("%s-etcd", cluster.Name),
	}

	if err := client.Get(ctx, etcdCAObjectKey, etcdCASecret); err == nil {
		hasEtcdCA = true
	} else if !apierrors.IsNotFound(err) {
		return nil, err
	}

    isAgentless := kcp.Spec.AgentlessConfig != nil

	return &ControlPlane{
		KCP:                  kcp,
		Cluster:              cluster,
		Machines:             ownedMachines,
		machinesPatchHelpers: patchHelpers,
		hasEtcdCA:            hasEtcdCA,
		KthreesConfigs:       kthreesConfigs,
		InfraResources:       infraObjects,
		reconciliationTime:   metav1.Now(),
        IsAgentless:          isAgentless,
	}, nil
}

// FailureDomains returns a slice of failure domain objects synced from the infrastructure provider into Cluster.Status.
func (c *ControlPlane) FailureDomains() clusterv1.FailureDomains {
	if c.Cluster.Status.FailureDomains == nil {
		return clusterv1.FailureDomains{}
	}
	return c.Cluster.Status.FailureDomains
}

// Version returns the KThreesControlPlane's version.
func (c *ControlPlane) Version() *string {
	return &c.KCP.Spec.Version
}

// InfrastructureTemplate returns the KThreesControlPlane's infrastructure template.
func (c *ControlPlane) InfrastructureTemplate() *corev1.ObjectReference {
	return &c.KCP.Spec.MachineTemplate.InfrastructureRef
}

// AsOwnerReference returns an owner reference to the KThreesControlPlane.
func (c *ControlPlane) AsOwnerReference() *metav1.OwnerReference {
	return &metav1.OwnerReference{
		APIVersion: controlplanev1.GroupVersion.String(),
		Kind:       "KThreesControlPlane",
		Name:       c.KCP.Name,
		UID:        c.KCP.UID,
	}
}

// EtcdImageData returns the etcd image data embedded in the ClusterConfiguration or empty strings if none are defined.
func (c *ControlPlane) EtcdImageData() (string, string) {
	return "", ""
}

// MachineInFailureDomainWithMostMachines returns the first matching failure domain with machines that has the most control-plane machines on it.
func (c *ControlPlane) MachineInFailureDomainWithMostMachines(ctx context.Context, machines collections.Machines) (*clusterv1.Machine, error) {
	fd := c.FailureDomainWithMostMachines(ctx, machines)
	machinesInFailureDomain := machines.Filter(collections.InFailureDomains(fd))
	machineToMark := machinesInFailureDomain.Oldest()
	if machineToMark == nil {
		return nil, ErrFailedToPickForDeletion
	}
	return machineToMark, nil
}

// MachineWithDeleteAnnotation returns a machine that has been annotated with DeleteMachineAnnotation key.
func (c *ControlPlane) MachineWithDeleteAnnotation(machines collections.Machines) collections.Machines {
	// See if there are any machines with DeleteMachineAnnotation key.
	annotatedMachines := machines.Filter(collections.HasAnnotationKey(clusterv1.DeleteMachineAnnotation))
	// If there are, return list of annotated machines.
	return annotatedMachines
}

// FailureDomainWithMostMachines returns a fd which exists both in machines and control-plane machines and has the most
// control-plane machines on it.
func (c *ControlPlane) FailureDomainWithMostMachines(ctx context.Context, machines collections.Machines) *string {
	// See if there are any Machines that are not in currently defined failure domains first.
	notInFailureDomains := machines.Filter(
		collections.Not(collections.InFailureDomains(c.FailureDomains().FilterControlPlane().GetIDs()...)),
	)
	if len(notInFailureDomains) > 0 {
		// return the failure domain for the oldest Machine not in the current list of failure domains
		// this could be either nil (no failure domain defined) or a failure domain that is no longer defined
		// in the cluster status.
		return notInFailureDomains.Oldest().Spec.FailureDomain
	}
	return failuredomains.PickMost(ctx, c.Cluster.Status.FailureDomains.FilterControlPlane(), c.Machines, machines)
}

// NextFailureDomainForScaleUp returns the failure domain with the fewest number of up-to-date machines.
func (c *ControlPlane) NextFailureDomainForScaleUp(ctx context.Context) *string {
	if len(c.Cluster.Status.FailureDomains.FilterControlPlane()) == 0 {
		return nil
	}
	return failuredomains.PickFewest(ctx, c.FailureDomains().FilterControlPlane(), c.UpToDateMachines())
}

// InitialControlPlaneConfig returns a new KThreesConfigSpec that is to be used for an initializing control plane.
func (c *ControlPlane) InitialControlPlaneConfig() *bootstrapv1.KThreesConfigSpec {
	bootstrapSpec := c.KCP.Spec.KThreesConfigSpec.DeepCopy()
	return bootstrapSpec
}

// JoinControlPlaneConfig returns a new KThreesConfigSpec that is to be used for joining control planes.
func (c *ControlPlane) JoinControlPlaneConfig() *bootstrapv1.KThreesConfigSpec {
	bootstrapSpec := c.KCP.Spec.KThreesConfigSpec.DeepCopy()
	return bootstrapSpec
}

// GenerateKThreesConfig generates a new KThreesConfig config for creating new control plane nodes.
func (c *ControlPlane) GenerateKThreesConfig(spec *bootstrapv1.KThreesConfigSpec) *bootstrapv1.KThreesConfig {
	// Create an owner reference without a controller reference because the owning controller is the machine controller
	owner := metav1.OwnerReference{
		APIVersion: controlplanev1.GroupVersion.String(),
		Kind:       "KThreesControlPlane",
		Name:       c.KCP.Name,
		UID:        c.KCP.UID,
	}

	bootstrapConfig := &bootstrapv1.KThreesConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name:            names.SimpleNameGenerator.GenerateName(c.KCP.Name + "-"),
			Namespace:       c.KCP.Namespace,
			Labels:          ControlPlaneLabelsForCluster(c.Cluster.Name, c.KCP.Spec.MachineTemplate),
			OwnerReferences: []metav1.OwnerReference{owner},
		},
		Spec: *spec,
	}
	return bootstrapConfig
}

// ControlPlaneLabelsForCluster returns a set of labels to add to a control plane machine for this specific cluster.
func ControlPlaneLabelsForCluster(clusterName string, machineTemplate controlplanev1.KThreesControlPlaneMachineTemplate) map[string]string {
	labels := make(map[string]string)
	for key, value := range machineTemplate.ObjectMeta.Labels {
		labels[key] = value
	}
	labels[clusterv1.ClusterNameLabel] = clusterName
	labels[clusterv1.MachineControlPlaneNameLabel] = ""
	labels[clusterv1.MachineControlPlaneLabel] = ""
	return labels
}

// NewMachine returns a machine configured to be a part of the control plane.
func (c *ControlPlane) NewMachine(infraRef, bootstrapRef *corev1.ObjectReference, failureDomain *string) *clusterv1.Machine {
	return &clusterv1.Machine{
		ObjectMeta: metav1.ObjectMeta{
			Name:      names.SimpleNameGenerator.GenerateName(c.KCP.Name + "-"),
			Namespace: c.KCP.Namespace,
			Labels:    ControlPlaneLabelsForCluster(c.Cluster.Name, c.KCP.Spec.MachineTemplate),
			OwnerReferences: []metav1.OwnerReference{
				*metav1.NewControllerRef(c.KCP, controlplanev1.GroupVersion.WithKind("KThreesControlPlane")),
			},
		},
		Spec: clusterv1.MachineSpec{
			ClusterName:       c.Cluster.Name,
			Version:           c.Version(),
			InfrastructureRef: *infraRef,
			Bootstrap: clusterv1.Bootstrap{
				ConfigRef: bootstrapRef,
			},
			FailureDomain: failureDomain,
		},
	}
}

// NeedsReplacementNode determines if the control plane needs to create a replacement node during upgrade.
func (c *ControlPlane) NeedsReplacementNode() bool {
	// Can't do anything with an unknown number of desired replicas.
	if c.KCP.Spec.Replicas == nil {
		return false
	}
	// if the number of existing machines is exactly 1 > than the number of replicas.
	return len(c.Machines)+1 == int(*c.KCP.Spec.Replicas)
}

// HasHealthyMachineStillProvisioning returns true if any healthy machine in the control plane is still in the process of being provisioned.
func (c *ControlPlane) HasHealthyMachineStillProvisioning() bool {
	return len(c.HealthyMachines().Filter(collections.Not(collections.HasNode()))) > 0
}

// HasDeletingMachine returns true if any machine in the control plane is in the process of being deleted.
func (c *ControlPlane) HasDeletingMachine() bool {
	return len(c.Machines.Filter(collections.HasDeletionTimestamp)) > 0
}

// MachinesNeedingRollout return a list of machines that need to be rolled out.
func (c *ControlPlane) MachinesNeedingRollout() collections.Machines {
	// Ignore machines to be deleted.
	machines := c.Machines.Filter(collections.Not(collections.HasDeletionTimestamp))

	// Return machines if they are scheduled for rollout or if with an outdated configuration.
	return machines.AnyFilter(
		// Machines that are scheduled for rollout (KCP.Spec.RolloutAfter set, the RolloutAfter deadline is expired, and the machine was created before the deadline).
		collections.ShouldRolloutAfter(&c.reconciliationTime, c.KCP.Spec.RolloutAfter),
		// Machines that do not match with KCP config.
		collections.Not(machinefilters.MatchesKCPConfiguration(c.InfraResources, c.KthreesConfigs, c.KCP)),
	)
}

// UpToDateMachines returns the machines that are up to date with the control
// plane's configuration and therefore do not require rollout.
func (c *ControlPlane) UpToDateMachines() collections.Machines {
	return c.Machines.Difference(c.MachinesNeedingRollout())
}

// getInfraResources fetches the external infrastructure resource for each machine in the collection and returns a map of machine.Name -> infraResource.
func getInfraResources(ctx context.Context, cl client.Client, machines collections.Machines) (map[string]*unstructured.Unstructured, error) {
	result := map[string]*unstructured.Unstructured{}
	for _, m := range machines {
		infraObj, err := external.Get(ctx, cl, &m.Spec.InfrastructureRef, m.Namespace)
		if err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return nil, fmt.Errorf("failed to retrieve infra obj for machine %q, %w", m.Name, err)
		}
		result[m.Name] = infraObj
	}
	return result, nil
}

// getKThreesConfigs fetches the k3s config for each machine in the collection and returns a map of machine.Name -> KThreesConfig.
func getKThreesConfigs(ctx context.Context, cl client.Client, machines collections.Machines) (map[string]*bootstrapv1.KThreesConfig, error) {
	result := map[string]*bootstrapv1.KThreesConfig{}
	for _, m := range machines {
		bootstrapRef := m.Spec.Bootstrap.ConfigRef
		if bootstrapRef == nil {
			continue
		}
		machineConfig := &bootstrapv1.KThreesConfig{}
		if err := cl.Get(ctx, client.ObjectKey{Name: bootstrapRef.Name, Namespace: m.Namespace}, machineConfig); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return nil, fmt.Errorf("failed to retrieve bootstrap config for machine %q: %w", m.Name, err)
		}
		result[m.Name] = machineConfig
	}
	return result, nil
}

// IsEtcdManaged returns true if the control plane relies on a managed etcd.
func (c *ControlPlane) IsEtcdManaged() bool {
	return c.KCP.Spec.KThreesConfigSpec.IsEtcdEmbedded() && c.hasEtcdCA
}

// UnhealthyMachines returns the list of control plane machines marked as unhealthy by MHC.
func (c *ControlPlane) UnhealthyMachines() collections.Machines {
	return c.Machines.Filter(collections.HasUnhealthyCondition)
}

// HealthyMachines returns the list of control plane machines not marked as unhealthy by MHC.
func (c *ControlPlane) HealthyMachines() collections.Machines {
	return c.Machines.Filter(collections.Not(collections.HasUnhealthyCondition))
}

// HasUnhealthyMachine returns true if any machine in the control plane is marked as unhealthy by MHC.
func (c *ControlPlane) HasUnhealthyMachine() bool {
	return len(c.UnhealthyMachines()) > 0
}

func (c *ControlPlane) PatchMachines(ctx context.Context) error {
	errList := []error{}
	for i := range c.Machines {
		machine := c.Machines[i]
		if helper, ok := c.machinesPatchHelpers[machine.Name]; ok {
			if err := helper.Patch(ctx, machine, patch.WithOwnedConditions{Conditions: []clusterv1.ConditionType{
				controlplanev1.MachineAgentHealthyCondition,
				controlplanev1.MachineEtcdMemberHealthyCondition,
			}}); err != nil {
				errList = append(errList, fmt.Errorf("failed to patch machine %s: %w", machine.Name, err))
			}
			continue
		}
		errList = append(errList, fmt.Errorf("machine: %s, %w", machine.Name, ErrFailedToCreatePatchHelper))
	}

	return kerrors.NewAggregate(errList)
}

// SetPatchHelpers updates the patch helpers.
func (c *ControlPlane) SetPatchHelpers(patchHelpers map[string]*patch.Helper) {
	if c.machinesPatchHelpers == nil {
		c.machinesPatchHelpers = map[string]*patch.Helper{}
	}
	for machineName, patchHelper := range patchHelpers {
		c.machinesPatchHelpers[machineName] = patchHelper
	}
}

func (c *ControlPlane) GetControlPlaneClusterObjectKey() (types.NamespacedName, error) {
    if c.KCP.Spec.AgentlessConfig == nil {
        return types.NamespacedName{}, errors.New("control plane is not agentless")
    }
    return types.NamespacedName{
        Namespace: c.KCP.Namespace,
        Name:      c.KCP.Spec.AgentlessConfig.ControlPlaneClusterName,
    }, nil
}

// AgentlessControlPlaneDeployment returns the control plane deployment object for an agentless control plane.
func (c *ControlPlane) CreateAgentlessControlPlaneDeployment(token *string) (*ControlPlaneAgentlessDeployment, error) {
    if c.KCP.Spec.AgentlessConfig == nil {
        return nil, errors.New("control plane is not agentless")
    }

    controlPlanePort := gatewayv1.PortNumber(6443)
    controlPlaneNamespace := gatewayv1.Namespace(c.KCP.Namespace)

    return &ControlPlaneAgentlessDeployment{
        Deployment: appsv1.Deployment{
            ObjectMeta: metav1.ObjectMeta{
                Namespace: c.KCP.Namespace,
                Name:      c.KCP.Name,
            },
            Spec: appsv1.DeploymentSpec{
                Selector: &metav1.LabelSelector{
                    MatchLabels: map[string]string{
                        clusterv1.ClusterNameLabel: c.Cluster.Name,
                    },
                },
                Template: corev1.PodTemplateSpec{
                    ObjectMeta: metav1.ObjectMeta{
                        Labels: map[string]string{
                            clusterv1.ClusterNameLabel: c.Cluster.Name,
                        },
                    },
                    Spec: corev1.PodSpec{
                        Containers: []corev1.Container{
                            {
                                Name:  "k3s",
                                Image: fmt.Sprintf("rancher/k3s:%s", strings.Replace(c.KCP.Spec.Version, "+", "-", -1)),
                                Args: []string{
                                    "server",
                                    "--disable-agent",
                                    "--tls-san",
                                    c.Cluster.Spec.ControlPlaneEndpoint.Host,                  
                                },
                                Env: []corev1.EnvVar{
                                    {
                                        Name: "K3S_TOKEN",
                                        Value: *token,
                                    },
                                },
                                Ports: []corev1.ContainerPort{
                                    {
                                        ContainerPort: 6443,
                                        Name: "kubernetes",
                                    },
                                },
                                Resources: corev1.ResourceRequirements{
                                    Limits: corev1.ResourceList{
                                        corev1.ResourceCPU: resource.MustParse("500m"),
                                        corev1.ResourceMemory: resource.MustParse("100Mi"),
                                    },
                                },
                                VolumeMounts: []corev1.VolumeMount{
                                    {
                                        Name: "server-ca",
                                        MountPath: "/var/lib/rancher/k3s/server/tls",
                                    },
                                    {
                                        Name: "client-ca",
                                        MountPath: "/var/lib/rancher/k3s/client/tls",
                                    },
                                },
                            },
                        },
                        Volumes: []corev1.Volume{
                            {
                                Name: "server-ca",
                                VolumeSource: corev1.VolumeSource{
                                    Secret: &corev1.SecretVolumeSource{
                                        SecretName: fmt.Sprintf("%s-ca", c.Cluster.Name),
                                        Items: []corev1.KeyToPath{
                                            {
                                                Key:  "tls.crt",
                                                Path: "server-ca.crt",
                                            },
                                            {
                                                Key:  "tls.key",
                                                Path: "server-ca.key",
                                            },
                                        },
                                    },
                                },
                            },
                            {
                                Name: "client-ca",
                                VolumeSource: corev1.VolumeSource{
                                    Secret: &corev1.SecretVolumeSource{
                                        SecretName: fmt.Sprintf("%s-cca", c.Cluster.Name),
                                        Items: []corev1.KeyToPath{
                                            {
                                                Key:  "tls.crt",
                                                Path: "client-ca.crt",
                                            },
                                            {
                                                Key:  "tls.key",
                                                Path: "client-ca.key",
                                            },
                                        },
                                    },
                                },
                            },
                        },
                    },

                },

            },
        },
        Service: corev1.Service{
            ObjectMeta: metav1.ObjectMeta{
                Namespace: c.KCP.Namespace,
                Name:      c.KCP.Name,
            },
            Spec: corev1.ServiceSpec{
                Selector: map[string]string{
                    clusterv1.ClusterNameLabel: c.Cluster.Name,
                },
                Ports: []corev1.ServicePort{
                    {
                        Name: "kubernetes",
                        TargetPort: intstr.FromString("kubernetes"),
                        Port: 6443,
                    },
                },
            },
        },
        TLSRoute: gatewayv1alpha2.TLSRoute{
            ObjectMeta: metav1.ObjectMeta{
                Namespace: c.KCP.Namespace,
                Name:      c.KCP.Name,
            },
            Spec: gatewayv1alpha2.TLSRouteSpec{
                Rules: []gatewayv1alpha2.TLSRouteRule{
                    {
                        BackendRefs: []gatewayv1.BackendRef{
                            {
                                BackendObjectReference: gatewayv1.BackendObjectReference{
                                    Name: gatewayv1.ObjectName(c.KCP.Name),
                                    Namespace: &controlPlaneNamespace,
                                    Port: &controlPlanePort,
                                },
                            },
                        },
                    },
                },
            },

        },
    },
    nil
}

// GetAgentlessControlPlaneDeployment returns the control plane deployment object for an agentless control plane.
func (c *ControlPlane) GetAgentlessControlPlaneDeployment(ctx context.Context, client client.Client, restConfig *rest.Config) (*ControlPlaneAgentlessDeployment, error) {
    if c.KCP.Spec.AgentlessConfig == nil {
        return nil, errors.New("control plane is not agentless")
    }

    deployment := &appsv1.Deployment{}
    deploymentKey := types.NamespacedName{
        Namespace: c.KCP.Namespace,
        Name:      c.KCP.Name,
    }
    if err := client.Get(ctx, deploymentKey, deployment); err != nil {
        if apierrors.IsNotFound(err) {
            return nil, nil
        }
        return nil, err
    }

    service := &corev1.Service{}
    serviceKey := types.NamespacedName{
        Namespace: c.KCP.Namespace,
        Name:      c.KCP.Name,
    }
    if err := client.Get(ctx, serviceKey, service); err != nil {
        if apierrors.IsNotFound(err) {
            return nil, nil
        }
        return nil, err
    }

    // get client for gateway api
    gatewayClients,err := clientset.NewForConfig(restConfig)
    if (err != nil) {
        return nil, err
    }
    gatewayv1alpha2Client := gatewayClients.GatewayV1alpha2()

    tlsRoute,err := gatewayv1alpha2Client.TLSRoutes(c.KCP.Namespace).Get(ctx, c.KCP.Name, metav1.GetOptions{})
    if err != nil {
        if apierrors.IsNotFound(err) {
            return nil, nil
        }
        return nil, err
    }

    return &ControlPlaneAgentlessDeployment{
        Deployment: *deployment,
        Service: *service,
        TLSRoute: *tlsRoute,
    }, nil
}

