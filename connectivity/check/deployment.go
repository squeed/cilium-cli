// SPDX-License-Identifier: Apache-2.0
// Copyright Authors of Cilium

package check

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	k8sErrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	"github.com/cilium/cilium-cli/defaults"
	"github.com/cilium/cilium-cli/k8s"
)

const (
	perfClientDeploymentName       = "perf-client"
	perfClientAcrossDeploymentName = "perf-client-other-node"
	perfServerDeploymentName       = "perf-server"

	perfHostNetNamingSuffix = "-host-net"

	clientDeploymentName  = "client"
	client2DeploymentName = "client2"

	DNSTestServerContainerName = "dns-test-server"

	echoSameNodeDeploymentName     = "echo-same-node"
	echoOtherNodeDeploymentName    = "echo-other-node"
	echoExternalNodeDeploymentName = "echo-external-node"
	corednsConfigMapName           = "coredns-configmap"
	corednsConfigVolumeName        = "coredns-config-volume"
	kindEchoName                   = "echo"
	kindEchoExternalNodeName       = "echo-external-node"
	kindClientName                 = "client"
	kindPerfName                   = "perf"

	hostNetNSDeploymentName = "host-netns"
	kindHostNetNS           = "host-netns"

	EchoServerHostPort = 40000

	IngressServiceName         = "ingress-service"
	ingressServiceInsecurePort = "31000"
	ingressServiceSecurePort   = "31001"
)

// perfDeploymentNameManager provides methods for building deployment names
// based on the given parameters.
// Names returned by the methods will be unique depending on how the deployments
// are expected to be modified for the configured test.
type perfDeploymentNameManager struct {
	clientDeploymentName       string
	clientAcrossDeploymentName string
	serverDeploymentName       string
}

func (nm *perfDeploymentNameManager) ClientName() string {
	return nm.clientDeploymentName
}

func (nm *perfDeploymentNameManager) ClientAcrossName() string {
	return nm.clientAcrossDeploymentName
}

func (nm *perfDeploymentNameManager) ServerName() string {
	return nm.serverDeploymentName
}

func newPerfDeploymentNameManager(params *Parameters) *perfDeploymentNameManager {
	suffix := ""
	if params.PerfHostNet {
		suffix = perfHostNetNamingSuffix
	}

	return &perfDeploymentNameManager{
		clientDeploymentName:       perfClientDeploymentName + suffix,
		clientAcrossDeploymentName: perfClientAcrossDeploymentName + suffix,
		serverDeploymentName:       perfServerDeploymentName + suffix,
	}
}

type deploymentParameters struct {
	Name           string
	Kind           string
	Image          string
	Replicas       int
	NamedPort      string
	Port           int
	HostPort       int
	Command        []string
	Affinity       *corev1.Affinity
	NodeSelector   map[string]string
	ReadinessProbe *corev1.Probe
	Labels         map[string]string
	HostNetwork    bool
	Tolerations    []corev1.Toleration
}

func newDeployment(p deploymentParameters) *appsv1.Deployment {
	if p.Replicas == 0 {
		p.Replicas = 1
	}
	if len(p.NamedPort) == 0 {
		p.NamedPort = fmt.Sprintf("port-%d", p.Port)
	}
	replicas32 := int32(p.Replicas)
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name: p.Name,
			Labels: map[string]string{
				"name": p.Name,
				"kind": p.Kind,
			},
		},
		Spec: appsv1.DeploymentSpec{
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Name: p.Name,
					Labels: map[string]string{
						"name": p.Name,
						"kind": p.Kind,
					},
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name: p.Name,
							Env: []corev1.EnvVar{
								{Name: "PORT", Value: fmt.Sprintf("%d", p.Port)},
								{Name: "NAMED_PORT", Value: p.NamedPort},
							},
							Ports: []corev1.ContainerPort{
								{Name: p.NamedPort, ContainerPort: int32(p.Port), HostPort: int32(p.HostPort)},
							},
							Image:           p.Image,
							ImagePullPolicy: corev1.PullIfNotPresent,
							Command:         p.Command,
							ReadinessProbe:  p.ReadinessProbe,
							SecurityContext: &corev1.SecurityContext{
								Capabilities: &corev1.Capabilities{
									Add: []corev1.Capability{"NET_RAW"},
								},
							},
						},
					},
					Affinity:           p.Affinity,
					NodeSelector:       p.NodeSelector,
					HostNetwork:        p.HostNetwork,
					Tolerations:        p.Tolerations,
					ServiceAccountName: p.Name,
				},
			},
			Replicas: &replicas32,
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{
					"name": p.Name,
					"kind": p.Kind,
				},
			},
		},
	}

	for k, v := range p.Labels {
		dep.Spec.Template.ObjectMeta.Labels[k] = v
	}

	return dep
}

func newDeploymentWithDNSTestServer(p deploymentParameters, DNSTestServerImage string) *appsv1.Deployment {
	dep := newDeployment(p)

	dep.Spec.Template.Spec.Containers = append(
		dep.Spec.Template.Spec.Containers,
		corev1.Container{
			Args: []string{"-conf", "/etc/coredns/Corefile"},
			Name: DNSTestServerContainerName,
			Ports: []corev1.ContainerPort{
				{ContainerPort: 53, Name: "dns-53"},
				{ContainerPort: 53, Name: "dns-udp-53", Protocol: corev1.ProtocolUDP},
			},
			Image:           DNSTestServerImage,
			ImagePullPolicy: corev1.PullIfNotPresent,
			ReadinessProbe:  newLocalReadinessProbe(8181, "/ready"),
			VolumeMounts: []corev1.VolumeMount{
				{
					Name:      corednsConfigVolumeName,
					MountPath: "/etc/coredns",
					ReadOnly:  true,
				},
			},
		},
	)
	dep.Spec.Template.Spec.Volumes = []corev1.Volume{
		{
			Name: corednsConfigVolumeName,
			VolumeSource: corev1.VolumeSource{
				ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{
						Name: corednsConfigMapName,
					},
					Items: []corev1.KeyToPath{
						{
							Key:  "Corefile",
							Path: "Corefile",
						},
					},
				},
			},
		},
	}

	return dep
}

type daemonSetParameters struct {
	Name           string
	Kind           string
	Image          string
	Replicas       int
	Port           int
	Command        []string
	Affinity       *corev1.Affinity
	ReadinessProbe *corev1.Probe
	Labels         map[string]string
	HostNetwork    bool
	Tolerations    []corev1.Toleration
}

func newDaemonSet(p daemonSetParameters) *appsv1.DaemonSet {
	ds := &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{
			Name: p.Name,
			Labels: map[string]string{
				"name": p.Name,
				"kind": p.Kind,
			},
		},
		Spec: appsv1.DaemonSetSpec{
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Name: p.Name,
					Labels: map[string]string{
						"name": p.Name,
						"kind": p.Kind,
					},
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:            p.Name,
							Image:           p.Image,
							ImagePullPolicy: corev1.PullIfNotPresent,
							Command:         p.Command,
							ReadinessProbe:  p.ReadinessProbe,
							SecurityContext: &corev1.SecurityContext{
								Capabilities: &corev1.Capabilities{
									Add: []corev1.Capability{"NET_RAW"},
								},
							},
						},
					},
					Affinity:    p.Affinity,
					HostNetwork: p.HostNetwork,
					Tolerations: p.Tolerations,
				},
			},
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{
					"name": p.Name,
					"kind": p.Kind,
				},
			},
		},
	}

	for k, v := range p.Labels {
		ds.Spec.Template.ObjectMeta.Labels[k] = v
	}

	return ds
}

var serviceLabels = map[string]string{
	"kind": kindEchoName,
}

func newService(name string, selector map[string]string, labels map[string]string, portName string, port int) *corev1.Service {
	ipFamPol := corev1.IPFamilyPolicyPreferDualStack
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:   name,
			Labels: labels,
		},
		Spec: corev1.ServiceSpec{
			Type: corev1.ServiceTypeNodePort,
			Ports: []corev1.ServicePort{
				{Name: portName, Port: int32(port)},
			},
			Selector:       selector,
			IPFamilyPolicy: &ipFamPol,
		},
	}
}

func newLocalReadinessProbe(port int, path string) *corev1.Probe {
	return &corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{
			HTTPGet: &corev1.HTTPGetAction{
				Path:   path,
				Port:   intstr.FromInt(port),
				Scheme: corev1.URISchemeHTTP,
			},
		},
		TimeoutSeconds:      int32(2),
		SuccessThreshold:    int32(1),
		PeriodSeconds:       int32(1),
		InitialDelaySeconds: int32(1),
		FailureThreshold:    int32(3),
	}
}

func newIngress() *networkingv1.Ingress {
	return &networkingv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{
			Name: IngressServiceName,
			Annotations: map[string]string{
				"ingress.cilium.io/loadbalancer-mode":  "dedicated",
				"ingress.cilium.io/service-type":       "NodePort",
				"ingress.cilium.io/insecure-node-port": ingressServiceInsecurePort,
				"ingress.cilium.io/secure-node-port":   ingressServiceSecurePort,
			},
		},
		Spec: networkingv1.IngressSpec{
			IngressClassName: func(in string) *string {
				return &in
			}(defaults.IngressClassName),
			Rules: []networkingv1.IngressRule{
				{
					IngressRuleValue: networkingv1.IngressRuleValue{
						HTTP: &networkingv1.HTTPIngressRuleValue{
							Paths: []networkingv1.HTTPIngressPath{
								{
									Path: "/",
									PathType: func() *networkingv1.PathType {
										pt := networkingv1.PathTypeImplementationSpecific
										return &pt
									}(),
									Backend: networkingv1.IngressBackend{
										Service: &networkingv1.IngressServiceBackend{
											Name: echoSameNodeDeploymentName,
											Port: networkingv1.ServiceBackendPort{
												Number: 8080,
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
	}
}

// deploy ensures the test Namespace, Services and Deployments are running on the cluster.
func (ct *ConnectivityTest) deploy(ctx context.Context) error {
	if ct.params.ForceDeploy {
		if err := ct.deleteDeployments(ctx, ct.clients.src); err != nil {
			return err
		}
	}

	_, err := ct.clients.src.GetNamespace(ctx, ct.params.TestNamespace, metav1.GetOptions{})
	if err != nil {
		ct.Logf("✨ [%s] Creating namespace %s for connectivity check...", ct.clients.src.ClusterName(), ct.params.TestNamespace)
		_, err = ct.clients.src.CreateNamespace(ctx, ct.params.TestNamespace, metav1.CreateOptions{})
		if err != nil {
			return fmt.Errorf("unable to create namespace %s: %s", ct.params.TestNamespace, err)
		}
	}

	if ct.params.Perf {
		// For performance workloads, we want to ensure the client/server are in the same zone
		// If a zone has > 1 node, use that zone
		zones := map[string]int{}
		zone := ""
		lz := ""
		n, hasNodes := ct.client.ListNodes(ctx, metav1.ListOptions{})
		if hasNodes != nil {
			return fmt.Errorf("unable to query nodes")
		}
		for _, l := range n.Items {
			if _, ok := zones[l.GetLabels()[corev1.LabelTopologyZone]]; ok {
				zone = l.GetLabels()[corev1.LabelTopologyZone]
				break
			}
			zones[l.GetLabels()[corev1.LabelTopologyZone]] = 1
			lz = l.GetLabels()[corev1.LabelTopologyZone]
		}
		// No zone had > 1, use the last zone.
		if zone == "" {
			ct.Warn("Each zone only has a single node - could impact the performance test results")
			zone = lz
		}

		if ct.params.PerfHostNet {
			ct.Info("Deploying Perf deployments using host networking")
		}

		nm := newPerfDeploymentNameManager(&ct.params)

		// Need to capture the IP of the Server Deployment, and pass to the client to execute benchmark
		_, err = ct.clients.src.GetDeployment(ctx, ct.params.TestNamespace, nm.ClientName(), metav1.GetOptions{})
		if err != nil {
			ct.Logf("✨ [%s] Deploying %s deployment...", ct.clients.src.ClusterName(), nm.ClientName())
			perfClientDeployment := newDeployment(deploymentParameters{
				Name:      nm.ClientName(),
				Kind:      kindPerfName,
				NamedPort: "http-80",
				Port:      80,
				Image:     ct.params.PerformanceImage,
				Labels: map[string]string{
					"client": "role",
				},
				Command: []string{"/bin/bash", "-c", "sleep 10000000"},
				Affinity: &corev1.Affinity{
					NodeAffinity: &corev1.NodeAffinity{
						PreferredDuringSchedulingIgnoredDuringExecution: []corev1.PreferredSchedulingTerm{
							{
								Weight: 100,
								Preference: corev1.NodeSelectorTerm{
									MatchExpressions: []corev1.NodeSelectorRequirement{
										{Key: corev1.LabelTopologyZone, Operator: corev1.NodeSelectorOpIn, Values: []string{zone}},
									},
								},
							},
						},
					},
				},
				NodeSelector: ct.params.NodeSelector,
				HostNetwork:  ct.params.PerfHostNet,
			})
			_, err = ct.clients.src.CreateServiceAccount(ctx, ct.params.TestNamespace, k8s.NewServiceAccount(nm.ClientName()), metav1.CreateOptions{})
			if err != nil {
				return fmt.Errorf("unable to create service account %s: %s", nm.ClientName(), err)
			}
			_, err = ct.clients.src.CreateDeployment(ctx, ct.params.TestNamespace, perfClientDeployment, metav1.CreateOptions{})
			if err != nil {
				return fmt.Errorf("unable to create deployment %s: %w", perfClientDeployment, err)
			}
		}

		_, err = ct.clients.src.GetDeployment(ctx, ct.params.TestNamespace, nm.ServerName(), metav1.GetOptions{})
		if err != nil {
			ct.Logf("✨ [%s] Deploying %s deployment...", ct.clients.src.ClusterName(), nm.ServerName())
			perfServerDeployment := newDeployment(deploymentParameters{
				Name: nm.ServerName(),
				Kind: kindPerfName,
				Labels: map[string]string{
					"server": "role",
				},
				Port:    5001,
				Image:   ct.params.PerformanceImage,
				Command: []string{"/bin/bash", "-c", "netserver;sleep 10000000"},
				Affinity: &corev1.Affinity{
					NodeAffinity: &corev1.NodeAffinity{
						PreferredDuringSchedulingIgnoredDuringExecution: []corev1.PreferredSchedulingTerm{
							{
								Weight: 100,
								Preference: corev1.NodeSelectorTerm{
									MatchExpressions: []corev1.NodeSelectorRequirement{
										{Key: corev1.LabelTopologyZone, Operator: corev1.NodeSelectorOpIn, Values: []string{zone}},
									},
								},
							},
						},
					},
					PodAffinity: &corev1.PodAffinity{
						RequiredDuringSchedulingIgnoredDuringExecution: []corev1.PodAffinityTerm{
							{
								LabelSelector: &metav1.LabelSelector{
									MatchExpressions: []metav1.LabelSelectorRequirement{
										{Key: "name", Operator: metav1.LabelSelectorOpIn, Values: []string{nm.ClientName()}},
									},
								},
								TopologyKey: corev1.LabelHostname,
							},
						},
					},
				},
				NodeSelector: ct.params.NodeSelector,
				HostNetwork:  ct.params.PerfHostNet,
			})
			_, err = ct.clients.src.CreateServiceAccount(ctx, ct.params.TestNamespace, k8s.NewServiceAccount(nm.ServerName()), metav1.CreateOptions{})
			if err != nil {
				return fmt.Errorf("unable to create service account %s: %s", nm.ServerName(), err)
			}

			_, err = ct.clients.src.CreateDeployment(ctx, ct.params.TestNamespace, perfServerDeployment, metav1.CreateOptions{})
			if err != nil {
				return fmt.Errorf("unable to create deployment %s: %w", perfServerDeployment, err)
			}
		}

		// Deploy second client on a different node
		if !ct.params.SingleNode {
			_, err := ct.clients.src.GetDeployment(ctx, ct.params.TestNamespace, nm.ClientAcrossName(), metav1.GetOptions{})
			if err != nil {
				ct.Logf("✨ [%s] Deploying %s deployment...", ct.clients.src.ClusterName(), nm.ClientAcrossName())
				perfOtherClientDeployment := newDeployment(deploymentParameters{
					Name: nm.ClientAcrossName(),
					Kind: kindPerfName,
					Port: 5001,
					Labels: map[string]string{
						"client": "role",
					},
					Image:   ct.params.PerformanceImage,
					Command: []string{"/bin/bash", "-c", "sleep 10000000"},
					Affinity: &corev1.Affinity{
						NodeAffinity: &corev1.NodeAffinity{
							PreferredDuringSchedulingIgnoredDuringExecution: []corev1.PreferredSchedulingTerm{
								{
									Weight: 100,
									Preference: corev1.NodeSelectorTerm{
										MatchExpressions: []corev1.NodeSelectorRequirement{
											{Key: corev1.LabelTopologyZone, Operator: corev1.NodeSelectorOpIn, Values: []string{zone}},
										},
									},
								},
							},
						},
						PodAntiAffinity: &corev1.PodAntiAffinity{
							PreferredDuringSchedulingIgnoredDuringExecution: []corev1.WeightedPodAffinityTerm{
								{Weight: 100, PodAffinityTerm: corev1.PodAffinityTerm{
									LabelSelector: &metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{
										{Key: "name", Operator: metav1.LabelSelectorOpIn, Values: []string{nm.ClientName()}}}},
									TopologyKey: corev1.LabelHostname}}}},
					},
					NodeSelector: ct.params.NodeSelector,
					HostNetwork:  ct.params.PerfHostNet,
				})
				_, err = ct.clients.src.CreateServiceAccount(ctx, ct.params.TestNamespace, k8s.NewServiceAccount(nm.ClientAcrossName()), metav1.CreateOptions{})
				if err != nil {
					return fmt.Errorf("unable to create service account %s: %s", nm.ClientAcrossName(), err)
				}

				_, err = ct.clients.src.CreateDeployment(ctx, ct.params.TestNamespace, perfOtherClientDeployment, metav1.CreateOptions{})
				if err != nil {
					return fmt.Errorf("unable to create deployment %s: %s", perfOtherClientDeployment, err)
				}
			}
		}
		return nil
	}

	if ct.params.MultiCluster != "" {
		if ct.params.ForceDeploy {
			if err := ct.deleteDeployments(ctx, ct.clients.dst); err != nil {
				return err
			}
		}

		_, err = ct.clients.dst.GetNamespace(ctx, ct.params.TestNamespace, metav1.GetOptions{})
		if err != nil {
			ct.Logf("✨ [%s] Creating namespace %s for connectivity check...", ct.clients.dst.ClusterName(), ct.params.TestNamespace)
			_, err = ct.clients.dst.CreateNamespace(ctx, ct.params.TestNamespace, metav1.CreateOptions{})
			if err != nil {
				return fmt.Errorf("unable to create namespace %s: %s", ct.params.TestNamespace, err)
			}
		}
	}

	_, err = ct.clients.src.GetService(ctx, ct.params.TestNamespace, echoSameNodeDeploymentName, metav1.GetOptions{})
	if err != nil {
		ct.Logf("✨ [%s] Deploying %s service...", ct.clients.src.ClusterName(), echoSameNodeDeploymentName)
		svc := newService(echoSameNodeDeploymentName, map[string]string{"name": echoSameNodeDeploymentName}, serviceLabels, "http", 8080)
		_, err = ct.clients.src.CreateService(ctx, ct.params.TestNamespace, svc, metav1.CreateOptions{})
		if err != nil {
			return err
		}
	}

	if ct.params.MultiCluster != "" {
		_, err = ct.clients.src.GetService(ctx, ct.params.TestNamespace, echoOtherNodeDeploymentName, metav1.GetOptions{})
		if err != nil {
			ct.Logf("✨ [%s] Deploying %s service...", ct.clients.src.ClusterName(), echoOtherNodeDeploymentName)
			svc := newService(echoOtherNodeDeploymentName, map[string]string{"name": echoOtherNodeDeploymentName}, serviceLabels, "http", 8080)
			svc.ObjectMeta.Annotations = map[string]string{}
			svc.ObjectMeta.Annotations["service.cilium.io/global"] = "true"
			svc.ObjectMeta.Annotations["io.cilium/global-service"] = "true"

			_, err = ct.clients.src.CreateService(ctx, ct.params.TestNamespace, svc, metav1.CreateOptions{})
			if err != nil {
				return err
			}
		}
	}

	hostPort := 0
	if ct.features[FeatureHostPort].Enabled {
		hostPort = EchoServerHostPort
	}
	dnsConfigMap := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name: corednsConfigMapName,
		},
		Data: map[string]string{
			"Corefile": `. {
				local
				ready
				log
			}`,
		},
	}
	_, err = ct.clients.src.GetConfigMap(ctx, ct.params.TestNamespace, corednsConfigMapName, metav1.GetOptions{})
	if err != nil {
		ct.Logf("✨ [%s] Deploying DNS test server configmap...", ct.clients.src.ClusterName())
		_, err = ct.clients.src.CreateConfigMap(ctx, ct.params.TestNamespace, dnsConfigMap, metav1.CreateOptions{})
		if err != nil {
			return fmt.Errorf("unable to create configmap %s: %s", corednsConfigMapName, err)
		}
	}
	if ct.params.MultiCluster != "" {
		_, err = ct.clients.dst.GetConfigMap(ctx, ct.params.TestNamespace, corednsConfigMapName, metav1.GetOptions{})
		if err != nil {
			ct.Logf("✨ [%s] Deploying DNS test server configmap...", ct.clients.dst.ClusterName())
			_, err = ct.clients.dst.CreateConfigMap(ctx, ct.params.TestNamespace, dnsConfigMap, metav1.CreateOptions{})
			if err != nil {
				return fmt.Errorf("unable to create configmap %s: %s", corednsConfigMapName, err)
			}
		}
	}

	_, err = ct.clients.src.GetDeployment(ctx, ct.params.TestNamespace, echoSameNodeDeploymentName, metav1.GetOptions{})
	if err != nil {
		ct.Logf("✨ [%s] Deploying same-node deployment...", ct.clients.src.ClusterName())
		containerPort := 8080
		echoDeployment := newDeploymentWithDNSTestServer(deploymentParameters{
			Name:      echoSameNodeDeploymentName,
			Kind:      kindEchoName,
			Port:      containerPort,
			NamedPort: "http-8080",
			HostPort:  hostPort,
			Image:     ct.params.JSONMockImage,
			Labels:    map[string]string{"other": "echo"},
			Affinity: &corev1.Affinity{
				PodAffinity: &corev1.PodAffinity{
					RequiredDuringSchedulingIgnoredDuringExecution: []corev1.PodAffinityTerm{
						{
							LabelSelector: &metav1.LabelSelector{
								MatchExpressions: []metav1.LabelSelectorRequirement{
									{Key: "name", Operator: metav1.LabelSelectorOpIn, Values: []string{clientDeploymentName}},
								},
							},
							TopologyKey: corev1.LabelHostname,
						},
					},
				},
			},
			ReadinessProbe: newLocalReadinessProbe(containerPort, "/"),
		}, ct.params.DNSTestServerImage)
		_, err = ct.clients.src.CreateServiceAccount(ctx, ct.params.TestNamespace, k8s.NewServiceAccount(echoSameNodeDeploymentName), metav1.CreateOptions{})
		if err != nil {
			return fmt.Errorf("unable to create service account %s: %s", echoSameNodeDeploymentName, err)
		}
		_, err = ct.clients.src.CreateDeployment(ctx, ct.params.TestNamespace, echoDeployment, metav1.CreateOptions{})
		if err != nil {
			return fmt.Errorf("unable to create deployment %s: %s", echoSameNodeDeploymentName, err)
		}
	}

	_, err = ct.clients.src.GetDeployment(ctx, ct.params.TestNamespace, clientDeploymentName, metav1.GetOptions{})
	if err != nil {
		ct.Logf("✨ [%s] Deploying %s deployment...", ct.clients.src.ClusterName(), clientDeploymentName)
		clientDeployment := newDeployment(deploymentParameters{
			Name:         clientDeploymentName,
			Kind:         kindClientName,
			NamedPort:    "http-8080",
			Port:         8080,
			Image:        ct.params.CurlImage,
			Command:      []string{"/bin/ash", "-c", "sleep 10000000"},
			NodeSelector: ct.params.NodeSelector,
		})
		_, err = ct.clients.src.CreateServiceAccount(ctx, ct.params.TestNamespace, k8s.NewServiceAccount(clientDeploymentName), metav1.CreateOptions{})
		if err != nil {
			return fmt.Errorf("unable to create service account %s: %s", clientDeploymentName, err)
		}
		_, err = ct.clients.src.CreateDeployment(ctx, ct.params.TestNamespace, clientDeployment, metav1.CreateOptions{})
		if err != nil {
			return fmt.Errorf("unable to create deployment %s: %s", clientDeploymentName, err)
		}
	}

	// 2nd client with label other=client
	_, err = ct.clients.src.GetDeployment(ctx, ct.params.TestNamespace, client2DeploymentName, metav1.GetOptions{})
	if err != nil {
		ct.Logf("✨ [%s] Deploying %s deployment...", ct.clients.src.ClusterName(), client2DeploymentName)
		clientDeployment := newDeployment(deploymentParameters{
			Name:      client2DeploymentName,
			Kind:      kindClientName,
			NamedPort: "http-8080",
			Port:      8080,
			Image:     ct.params.CurlImage,
			Command:   []string{"/bin/ash", "-c", "sleep 10000000"},
			Labels:    map[string]string{"other": "client"},
			Affinity: &corev1.Affinity{
				PodAffinity: &corev1.PodAffinity{
					RequiredDuringSchedulingIgnoredDuringExecution: []corev1.PodAffinityTerm{
						{
							LabelSelector: &metav1.LabelSelector{
								MatchExpressions: []metav1.LabelSelectorRequirement{
									{Key: "name", Operator: metav1.LabelSelectorOpIn, Values: []string{clientDeploymentName}},
								},
							},
							TopologyKey: corev1.LabelHostname,
						},
					},
				},
			},
			NodeSelector: ct.params.NodeSelector,
		})
		_, err = ct.clients.src.CreateServiceAccount(ctx, ct.params.TestNamespace, k8s.NewServiceAccount(client2DeploymentName), metav1.CreateOptions{})
		if err != nil {
			return fmt.Errorf("unable to create service account %s: %s", client2DeploymentName, err)
		}
		_, err = ct.clients.src.CreateDeployment(ctx, ct.params.TestNamespace, clientDeployment, metav1.CreateOptions{})
		if err != nil {
			return fmt.Errorf("unable to create deployment %s: %s", client2DeploymentName, err)
		}
	}

	if !ct.params.SingleNode || ct.params.MultiCluster != "" {
		_, err = ct.clients.dst.GetService(ctx, ct.params.TestNamespace, echoOtherNodeDeploymentName, metav1.GetOptions{})
		if err != nil {
			ct.Logf("✨ [%s] Deploying echo-other-node service...", ct.clients.dst.ClusterName())
			svc := newService(echoOtherNodeDeploymentName, map[string]string{"name": echoOtherNodeDeploymentName}, serviceLabels, "http", 8080)

			if ct.params.MultiCluster != "" {
				svc.ObjectMeta.Annotations = map[string]string{}
				svc.ObjectMeta.Annotations["service.cilium.io/global"] = "true"
				svc.ObjectMeta.Annotations["io.cilium/global-service"] = "true"
			}

			_, err = ct.clients.dst.CreateService(ctx, ct.params.TestNamespace, svc, metav1.CreateOptions{})
			if err != nil {
				return err
			}
		}

		_, err = ct.clients.dst.GetDeployment(ctx, ct.params.TestNamespace, echoOtherNodeDeploymentName, metav1.GetOptions{})
		if err != nil {
			ct.Logf("✨ [%s] Deploying other-node deployment...", ct.clients.dst.ClusterName())
			containerPort := 8080
			echoOtherNodeDeployment := newDeploymentWithDNSTestServer(deploymentParameters{
				Name:      echoOtherNodeDeploymentName,
				Kind:      kindEchoName,
				NamedPort: "http-8080",
				Port:      containerPort,
				HostPort:  hostPort,
				Image:     ct.params.JSONMockImage,
				Labels:    map[string]string{"first": "echo"},
				Affinity: &corev1.Affinity{
					PodAntiAffinity: &corev1.PodAntiAffinity{
						RequiredDuringSchedulingIgnoredDuringExecution: []corev1.PodAffinityTerm{
							{
								LabelSelector: &metav1.LabelSelector{
									MatchExpressions: []metav1.LabelSelectorRequirement{
										{Key: "name", Operator: metav1.LabelSelectorOpIn, Values: []string{clientDeploymentName}},
									},
								},
								TopologyKey: corev1.LabelHostname,
							},
						},
					},
				},
				NodeSelector:   ct.params.NodeSelector,
				ReadinessProbe: newLocalReadinessProbe(containerPort, "/"),
			}, ct.params.DNSTestServerImage)
			_, err = ct.clients.dst.CreateServiceAccount(ctx, ct.params.TestNamespace, k8s.NewServiceAccount(echoOtherNodeDeploymentName), metav1.CreateOptions{})
			if err != nil {
				return fmt.Errorf("unable to create service account %s: %s", echoOtherNodeDeploymentName, err)
			}
			_, err = ct.clients.dst.CreateDeployment(ctx, ct.params.TestNamespace, echoOtherNodeDeployment, metav1.CreateOptions{})
			if err != nil {
				return fmt.Errorf("unable to create deployment %s: %w", echoOtherNodeDeploymentName, err)
			}
		}

		if ct.features[FeatureNodeWithoutCilium].Enabled {
			_, err = ct.clients.src.GetDaemonSet(ctx, ct.params.TestNamespace, hostNetNSDeploymentName, metav1.GetOptions{})
			if err != nil {
				ct.Logf("✨ [%s] Deploying host-netns daemonset...", ct.clients.src.ClusterName())
				ds := newDaemonSet(daemonSetParameters{
					Name:        hostNetNSDeploymentName,
					Kind:        kindHostNetNS,
					Image:       ct.params.CurlImage,
					Port:        8080,
					Labels:      map[string]string{"other": "host-netns"},
					Command:     []string{"/bin/ash", "-c", "sleep 10000000"},
					HostNetwork: true,
					Tolerations: []corev1.Toleration{
						{Operator: corev1.TolerationOpExists},
					},
				})
				_, err = ct.clients.src.CreateDaemonSet(ctx, ct.params.TestNamespace, ds, metav1.CreateOptions{})
				if err != nil {
					return fmt.Errorf("unable to create daemonset %s: %w", hostNetNSDeploymentName, err)
				}
			}

			_, err = ct.clients.src.GetDeployment(ctx, ct.params.TestNamespace, echoExternalNodeDeploymentName, metav1.GetOptions{})
			if err != nil {
				ct.Logf("✨ [%s] Deploying echo-external-node deployment...", ct.clients.src.ClusterName())
				containerPort := 8080
				echoExternalDeployment := newDeployment(deploymentParameters{
					Name:           echoExternalNodeDeploymentName,
					Kind:           kindEchoExternalNodeName,
					Port:           containerPort,
					NamedPort:      "http-8080",
					HostPort:       8080,
					Image:          ct.params.JSONMockImage,
					Labels:         map[string]string{"external": "echo"},
					NodeSelector:   map[string]string{"cilium.io/no-schedule": "true"},
					ReadinessProbe: newLocalReadinessProbe(containerPort, "/"),
					HostNetwork:    true,
					Tolerations: []corev1.Toleration{
						{Operator: corev1.TolerationOpExists},
					},
				})
				_, err = ct.clients.src.CreateServiceAccount(ctx, ct.params.TestNamespace, k8s.NewServiceAccount(echoExternalNodeDeploymentName), metav1.CreateOptions{})
				if err != nil {
					return fmt.Errorf("unable to create service account %s: %s", echoExternalNodeDeploymentName, err)
				}
				_, err = ct.clients.src.CreateDeployment(ctx, ct.params.TestNamespace, echoExternalDeployment, metav1.CreateOptions{})
				if err != nil {
					return fmt.Errorf("unable to create deployment %s: %s", echoExternalNodeDeploymentName, err)
				}
			}
		}
	}

	// Create one Ingress service for echo deployment
	if ct.features[FeatureIngressController].Enabled {
		_, err = ct.clients.src.GetIngress(ctx, ct.params.TestNamespace, IngressServiceName, metav1.GetOptions{})
		if err != nil {
			ct.Logf("✨ [%s] Deploying Ingress resource...", ct.clients.src.ClusterName())
			_, err = ct.clients.src.CreateIngress(ctx, ct.params.TestNamespace, newIngress(), metav1.CreateOptions{})
			if err != nil {
				return err
			}

			ingressServiceName := fmt.Sprintf("cilium-ingress-%s", IngressServiceName)
			ct.ingressService[ingressServiceName] = Service{
				Service: &corev1.Service{
					ObjectMeta: metav1.ObjectMeta{
						Name: ingressServiceName,
					},
					Spec: corev1.ServiceSpec{
						Ports: []corev1.ServicePort{
							{
								Name:     "http",
								Protocol: corev1.ProtocolTCP,
								Port:     80,
							},
							{
								Name:     "https",
								Protocol: corev1.ProtocolTCP,
								Port:     443,
							},
						},
					},
				},
			}
		}
	}
	return nil
}

// deploymentList returns 2 lists of Deployments to be used for running tests with.
func (ct *ConnectivityTest) deploymentList() (srcList []string, dstList []string) {
	if !ct.params.Perf {
		srcList = []string{clientDeploymentName, client2DeploymentName, echoSameNodeDeploymentName}
	} else {
		perfNm := newPerfDeploymentNameManager(&ct.params)
		srcList = []string{perfNm.ClientName(), perfNm.ServerName()}
		if !ct.params.SingleNode {
			srcList = append(srcList, perfNm.ClientAcrossName())
		}
	}

	if (ct.params.MultiCluster != "" || !ct.params.SingleNode) && !ct.params.Perf {
		dstList = append(dstList, echoOtherNodeDeploymentName)
	}

	if ct.features[FeatureNodeWithoutCilium].Enabled {
		dstList = append(dstList, echoExternalNodeDeploymentName)
	}

	return srcList, dstList
}

func (ct *ConnectivityTest) deleteDeployments(ctx context.Context, client *k8s.Client) error {
	ct.Logf("🔥 [%s] Deleting connectivity check deployments...", client.ClusterName())
	_ = client.DeleteDeployment(ctx, ct.params.TestNamespace, echoSameNodeDeploymentName, metav1.DeleteOptions{})
	_ = client.DeleteDeployment(ctx, ct.params.TestNamespace, echoOtherNodeDeploymentName, metav1.DeleteOptions{})
	_ = client.DeleteDeployment(ctx, ct.params.TestNamespace, clientDeploymentName, metav1.DeleteOptions{})
	_ = client.DeleteDeployment(ctx, ct.params.TestNamespace, client2DeploymentName, metav1.DeleteOptions{})
	_ = client.DeleteServiceAccount(ctx, ct.params.TestNamespace, echoSameNodeDeploymentName, metav1.DeleteOptions{})
	_ = client.DeleteServiceAccount(ctx, ct.params.TestNamespace, echoOtherNodeDeploymentName, metav1.DeleteOptions{})
	_ = client.DeleteServiceAccount(ctx, ct.params.TestNamespace, clientDeploymentName, metav1.DeleteOptions{})
	_ = client.DeleteServiceAccount(ctx, ct.params.TestNamespace, client2DeploymentName, metav1.DeleteOptions{})
	_ = client.DeleteService(ctx, ct.params.TestNamespace, echoSameNodeDeploymentName, metav1.DeleteOptions{})
	_ = client.DeleteService(ctx, ct.params.TestNamespace, echoOtherNodeDeploymentName, metav1.DeleteOptions{})
	_ = client.DeleteConfigMap(ctx, ct.params.TestNamespace, corednsConfigMapName, metav1.DeleteOptions{})
	_ = client.DeleteNamespace(ctx, ct.params.TestNamespace, metav1.DeleteOptions{})

	_, err := client.GetNamespace(ctx, ct.params.TestNamespace, metav1.GetOptions{})
	if err == nil {
		ct.Logf("⌛ [%s] Waiting for namespace %s to disappear", client.ClusterName(), ct.params.TestNamespace)
		for err == nil {
			time.Sleep(time.Second)
			// Retry the namespace deletion in-case the previous delete was
			// rejected, i.e. by yahoo/k8s-namespace-guard
			_ = client.DeleteNamespace(ctx, ct.params.TestNamespace, metav1.DeleteOptions{})
			_, err = client.GetNamespace(ctx, ct.params.TestNamespace, metav1.GetOptions{})
		}
	}

	return nil
}

// validateDeployment checks if the Deployments we created have the expected Pods in them.
func (ct *ConnectivityTest) validateDeployment(ctx context.Context) error {

	ct.Debug("Validating Deployments...")

	srcDeployments, dstDeployments := ct.deploymentList()
	if len(srcDeployments) > 0 {
		if err := ct.waitForDeployments(ctx, ct.clients.src, srcDeployments); err != nil {
			return err
		}
	}
	if len(dstDeployments) > 0 {
		if err := ct.waitForDeployments(ctx, ct.clients.dst, dstDeployments); err != nil {
			return err
		}
	}

	if ct.params.Perf {
		perfPods, err := ct.client.ListPods(ctx, ct.params.TestNamespace, metav1.ListOptions{LabelSelector: "kind=" + kindPerfName})
		if err != nil {
			return fmt.Errorf("unable to list perf pods: %w", err)
		}
		for _, perfPod := range perfPods.Items {
			// Filter out existing perf pods in cilium-test based on scenario
			if ct.params.PerfHostNet != perfPod.Spec.HostNetwork {
				continue
			}

			// Individual endpoints will not be created for pods using node's network stack
			if !ct.params.PerfHostNet {
				ctx, cancel := context.WithTimeout(ctx, ct.params.ciliumEndpointTimeout())
				defer cancel()
				if err := ct.waitForCiliumEndpoint(ctx, ct.clients.src, ct.params.TestNamespace, perfPod.Name); err != nil {
					return err
				}
			}
			_, hasLabel := perfPod.GetLabels()["server"]
			if hasLabel {
				ct.perfServerPod[perfPod.Name] = Pod{
					K8sClient: ct.client,
					Pod:       perfPod.DeepCopy(),
					port:      5201,
				}
			} else {
				ct.perfClientPods[perfPod.Name] = Pod{
					K8sClient: ct.client,
					Pod:       perfPod.DeepCopy(),
				}
			}
		}
		return nil
	}

	clientPods, err := ct.client.ListPods(ctx, ct.params.TestNamespace, metav1.ListOptions{LabelSelector: "kind=" + kindClientName})
	if err != nil {
		return fmt.Errorf("unable to list client pods: %s", err)
	}

	for _, pod := range clientPods.Items {
		ctx, cancel := context.WithTimeout(ctx, ct.params.ciliumEndpointTimeout())
		defer cancel()
		if err := ct.waitForCiliumEndpoint(ctx, ct.clients.src, ct.params.TestNamespace, pod.Name); err != nil {
			return err
		}

		ct.clientPods[pod.Name] = Pod{
			K8sClient: ct.client,
			Pod:       pod.DeepCopy(),
		}
	}

	sameNodePods, err := ct.clients.src.ListPods(ctx, ct.params.TestNamespace, metav1.ListOptions{LabelSelector: "name=" + echoSameNodeDeploymentName})
	if err != nil {
		return fmt.Errorf("unable to list same node pods: %w", err)
	}
	if len(sameNodePods.Items) != 1 {
		return fmt.Errorf("unexpected number of same node pods: %d", len(sameNodePods.Items))
	}
	sameNodePod := Pod{
		Pod: sameNodePods.Items[0].DeepCopy(),
	}

	sameNodeDNSCtx, sameNodeDNSCancel := context.WithTimeout(ctx, ct.params.ipCacheTimeout())
	defer sameNodeDNSCancel()
	for _, cp := range ct.clientPods {
		err := ct.waitForPodDNS(sameNodeDNSCtx, cp, sameNodePod)
		if err != nil {
			return err
		}
	}

	if !ct.params.SingleNode || ct.params.MultiCluster != "" {
		otherNodePods, err := ct.clients.dst.ListPods(ctx, ct.params.TestNamespace, metav1.ListOptions{LabelSelector: "name=" + echoOtherNodeDeploymentName})
		if err != nil {
			return fmt.Errorf("unable to list other node pods: %w", err)
		}
		if len(otherNodePods.Items) != 1 {
			return fmt.Errorf("unexpected number of other node pods: %d", len(otherNodePods.Items))
		}
		otherNodePod := Pod{
			Pod: otherNodePods.Items[0].DeepCopy(),
		}

		otherNodeDNSCtx, otherNodeDNSCancel := context.WithTimeout(ctx, ct.params.ipCacheTimeout())
		defer otherNodeDNSCancel()
		for _, cp := range ct.clientPods {
			err := ct.waitForPodDNS(otherNodeDNSCtx, cp, otherNodePod)
			if err != nil {
				return err
			}
		}
	}

	if ct.features[FeatureNodeWithoutCilium].Enabled {
		echoExternalNodePods, err := ct.clients.dst.ListPods(ctx, ct.params.TestNamespace, metav1.ListOptions{LabelSelector: "name=" + echoExternalNodeDeploymentName})
		if err != nil {
			return fmt.Errorf("unable to list other node pods: %w", err)
		}

		for _, pod := range echoExternalNodePods.Items {
			ct.echoExternalPods[pod.Name] = Pod{
				K8sClient: ct.client,
				Pod:       pod.DeepCopy(),
				scheme:    "http",
				port:      8080, // listen port of the echo server inside the container
			}
		}
	}

	svcDNSCtx, svcDNSCancel := context.WithTimeout(ctx, ct.params.ipCacheTimeout())
	defer svcDNSCancel()
	for _, cp := range ct.clientPods {
		err := ct.waitForServiceDNS(svcDNSCtx, cp)
		if err != nil {
			return err
		}
	}

	for _, client := range ct.clients.clients() {
		echoPods, err := client.ListPods(ctx, ct.params.TestNamespace, metav1.ListOptions{LabelSelector: "kind=" + kindEchoName})
		if err != nil {
			return fmt.Errorf("unable to list echo pods: %w", err)
		}
		for _, echoPod := range echoPods.Items {
			ctx, cancel := context.WithTimeout(ctx, ct.params.ciliumEndpointTimeout())
			defer cancel()
			if err := ct.waitForCiliumEndpoint(ctx, client, ct.params.TestNamespace, echoPod.Name); err != nil {
				return err
			}

			ct.echoPods[echoPod.Name] = Pod{
				K8sClient: client,
				Pod:       echoPod.DeepCopy(),
				scheme:    "http",
				port:      8080, // listen port of the echo server inside the container
			}
		}
	}

	for _, client := range ct.clients.clients() {
		echoServices, err := client.ListServices(ctx, ct.params.TestNamespace, metav1.ListOptions{LabelSelector: "kind=" + kindEchoName})
		if err != nil {
			return fmt.Errorf("unable to list echo services: %w", err)
		}

		for _, echoService := range echoServices.Items {
			if ct.params.MultiCluster != "" {
				if _, exists := ct.echoServices[echoService.Name]; exists {
					// ct.clients.clients() lists the client cluster first.
					// If we already have this service (for the client cluster), keep it
					// so that we can rely on the service's ClusterIP being valid for the
					// client pods.
					continue
				}
			}

			ct.echoServices[echoService.Name] = Service{
				Service: echoService.DeepCopy(),
			}
		}
	}

	for _, s := range ct.echoServices {
		if err := ct.waitForService(ctx, s); err != nil {
			return err
		}
	}

	if ct.features[FeatureIngressController].Enabled {
		ingressServices, err := ct.clients.src.ListServices(ctx, ct.params.TestNamespace, metav1.ListOptions{LabelSelector: "cilium.io/ingress=true"})
		if err != nil {
			return fmt.Errorf("unable to list ingress services: %w", err)
		}

		for _, ingressService := range ingressServices.Items {
			ct.ingressService[ingressService.Name] = Service{
				Service: ingressService.DeepCopy(),
			}
		}
	}

	if ct.params.MultiCluster == "" {
		for _, ciliumPod := range ct.ciliumPods {
			hostIP := ciliumPod.Pod.Status.HostIP
			for _, s := range ct.echoServices {
				if err := ct.waitForNodePorts(ctx, hostIP, s); err != nil {
					return err
				}
			}
		}
	}

	hostNetNSPods, err := ct.client.ListPods(ctx, ct.params.TestNamespace, metav1.ListOptions{LabelSelector: "kind=" + kindHostNetNS})
	if err != nil {
		return fmt.Errorf("unable to list host netns pods: %w", err)
	}

	for _, pod := range hostNetNSPods.Items {
		ct.hostNetNSPodsByNode[pod.Spec.NodeName] = Pod{
			K8sClient: ct.client,
			Pod:       pod.DeepCopy(),
		}
	}

	var logOnce sync.Once
	for _, client := range ct.clients.clients() {
		externalWorkloads, err := client.ListCiliumExternalWorkloads(ctx, metav1.ListOptions{})
		if k8sErrors.IsNotFound(err) {
			logOnce.Do(func() {
				ct.Log("ciliumexternalworkloads.cilium.io is not defined. Disabling external workload tests")
			})
			continue
		} else if err != nil {
			return fmt.Errorf("unable to list external workloads: %w", err)
		}
		for _, externalWorkload := range externalWorkloads.Items {
			ct.externalWorkloads[externalWorkload.Name] = ExternalWorkload{
				workload: externalWorkload.DeepCopy(),
			}
		}
	}

	// TODO: unconditionally re-enable the IPCache check once
	// https://github.com/cilium/cilium-cli/issues/361 is resolved.
	if ct.params.SkipIPCacheCheck {
		ct.Infof("Skipping IPCache check")
	} else {
		// Set the timeout for all IP cache lookup retries
		ipCacheCtx, cancel := context.WithTimeout(ctx, ct.params.ipCacheTimeout())
		defer cancel()
		for _, cp := range ct.ciliumPods {
			if err := ct.waitForIPCache(ipCacheCtx, cp); err != nil {
				return err
			}
		}
	}

	return nil
}

// Validate that srcPod can query the DNS server on dstPod successfully
func (ct *ConnectivityTest) waitForPodDNS(ctx context.Context, srcPod, dstPod Pod) error {
	ct.Logf("⌛ [%s] Waiting for pod %s to reach DNS server on %s pod...", ct.client.ClusterName(), srcPod.Name(), dstPod.Name())

	for {
		// Don't retry lookups more often than once per second.
		r := time.After(time.Second)

		// We don't care about the actual response content, we just want to check the DNS operativity.
		// Since the coreDNS test server has been deployed with the "local" plugin enabled,
		// we query it with a so-called "local request" (e.g. "localhost") to get a response.
		// See https://coredns.io/plugins/local/ for more info.
		target := "localhost"
		stdout, err := srcPod.K8sClient.ExecInPod(ctx, srcPod.Pod.Namespace, srcPod.Pod.Name,
			"", []string{"nslookup", target, dstPod.Address(IPFamilyAny)})

		if err == nil {
			return nil
		}

		ct.Debugf("Error looking up %s from pod %s to server on pod %s: %s: %s", target, srcPod.Name(), dstPod.Name(), err, stdout.String())

		select {
		case <-ctx.Done():
			return fmt.Errorf("timeout reached waiting lookup for %s from pod %s to server on pod %s to succeed (last error: %w)",
				target, srcPod.Name(), dstPod.Name(), err,
			)
		default:
		}

		// Wait for the pace timer to avoid busy polling.
		<-r
	}
}

// Validate that kube-dns responds and knows about cluster services
func (ct *ConnectivityTest) waitForServiceDNS(ctx context.Context, pod Pod) error {
	ct.Logf("⌛ [%s] Waiting for pod %s to reach default/kubernetes service...", ct.client.ClusterName(), pod.Name())

	for {
		// Don't retry lookups more often than once per second.
		r := time.After(time.Second)

		target := "kubernetes.default"
		stdout, err := pod.K8sClient.ExecInPod(ctx, pod.Pod.Namespace, pod.Pod.Name,
			"", []string{"nslookup", target})
		if err == nil {
			return nil
		}

		ct.Debugf("Error looking up %s from pod %s: %s: %s", target, pod.Name(), err, stdout.String())

		select {
		case <-ctx.Done():
			return fmt.Errorf("timeout reached waiting lookup for %s from pod %s to succeed (last error: %w)", target, pod.Name(), err)
		default:
		}

		// Wait for the pace timer to avoid busy polling.
		<-r
	}
}

func (ct *ConnectivityTest) waitForIPCache(ctx context.Context, pod Pod) error {
	ct.Logf("⌛ [%s] Waiting for Cilium pod %s to have all the pod IPs in eBPF ipcache...", ct.client.ClusterName(), pod.Name())

	for {
		// Don't retry lookups more often than once per second.
		r := time.After(time.Second)

		err := ct.validateIPCache(ctx, pod)
		if err == nil {
			ct.Debug("Successfully validated all podIDs in ipcache")
			return nil
		}

		ct.Debugf("Error validating all podIDs in ipcache: %s, retrying...", err)

		select {
		case <-ctx.Done():
			return fmt.Errorf("timeout reached waiting for pod IDs in ipcache of Cilium pod %s (last error: %w)", pod.Name(), err)
		default:
		}

		// Wait for the pace timer to avoid busy polling.
		<-r
	}
}

func (ct *ConnectivityTest) validateIPCache(ctx context.Context, agentPod Pod) error {
	stdout, err := agentPod.K8sClient.ExecInPod(ctx, agentPod.Pod.Namespace, agentPod.Pod.Name,
		defaults.AgentContainerName, []string{"cilium", "bpf", "ipcache", "list", "-o", "json"})
	if err != nil {
		return fmt.Errorf("failed to list ipcache bpf map: %w", err)
	}

	var ic ipCache

	if err := json.Unmarshal(stdout.Bytes(), &ic); err != nil {
		return fmt.Errorf("failed to unmarshal Cilium ipcache stdout json: %w", err)
	}

	for _, p := range ct.clientPods {
		if _, err := ic.findPodID(p); err != nil {
			return fmt.Errorf("couldn't find client Pod %v in ipcache: %w", p, err)
		}
	}

	for _, p := range ct.echoPods {
		if _, err := ic.findPodID(p); err != nil {
			return fmt.Errorf("couldn't find echo Pod %v in ipcache: %w", p, err)
		}
	}

	return nil
}

func (ct *ConnectivityTest) waitForDeployments(ctx context.Context, client *k8s.Client, deployments []string) error {
	ct.Logf("⌛ [%s] Waiting for deployments %s to become ready...", client.ClusterName(), deployments)

	ctx, cancel := context.WithTimeout(ctx, ct.params.podReadyTimeout())
	defer cancel()
	for _, name := range deployments {
		for {
			err := client.CheckDeploymentStatus(ctx, ct.params.TestNamespace, name)
			if err == nil {
				break
			}
			select {
			case <-time.After(time.Second):
			case <-ctx.Done():
				return fmt.Errorf("waiting for deployment %s to become ready has been interrupted: %w (last error: %s)", name, ctx.Err(), err)
			}
		}
	}

	return nil
}

func (ct *ConnectivityTest) waitForService(ctx context.Context, service Service) error {
	ct.Logf("⌛ [%s] Waiting for Service %s to become ready...", ct.client.ClusterName(), service.Name())

	// Retry the service lookup for the duration of the ready context.
	ctx, cancel := context.WithTimeout(ctx, ct.params.serviceReadyTimeout())
	defer cancel()

	pod := ct.RandomClientPod()
	if pod == nil {
		return fmt.Errorf("no client pod available")
	}

	for {
		// Don't retry lookups more often than once per second.
		r := time.After(time.Second)

		stdout, err := ct.client.ExecInPod(ctx,
			pod.Pod.Namespace, pod.Pod.Name, pod.Pod.Labels["name"],
			[]string{"nslookup", service.Service.Name}) // BusyBox nslookup doesn't support any arguments.

		// Lookup successful.
		if err == nil {
			svcIP := ""
			switch service.Service.Spec.Type {
			case corev1.ServiceTypeClusterIP, corev1.ServiceTypeNodePort:
				svcIP = service.Service.Spec.ClusterIP
			case corev1.ServiceTypeLoadBalancer:
				if len(service.Service.Status.LoadBalancer.Ingress) > 0 {
					svcIP = service.Service.Status.LoadBalancer.Ingress[0].IP
				}
			}
			if svcIP == "" {
				return nil
			}

			nslookupStr := strings.ReplaceAll(stdout.String(), "\r\n", "\n")
			if strings.Contains(nslookupStr, "Address: "+svcIP+"\n") {
				return nil
			}
			err = fmt.Errorf("Service IP %q not found in nslookup output %q", svcIP, nslookupStr)
		}

		ct.Debugf("Error waiting for service %s: %s: %s", service.Name(), err, stdout.String())

		select {
		case <-ctx.Done():
			return fmt.Errorf("timeout reached waiting for service %s (last error: %w)", service.Name(), err)
		default:
		}

		// Wait for the pace timer to avoid busy polling.
		<-r
	}
}

// waitForNodePorts waits until all the nodeports in a service are available on a given node.
func (ct *ConnectivityTest) waitForNodePorts(ctx context.Context, nodeIP string, service Service) error {
	pod := ct.RandomClientPod()
	if pod == nil {
		return fmt.Errorf("no client pod available")
	}
	ctx, cancel := context.WithTimeout(ctx, ct.params.serviceReadyTimeout())
	defer cancel()

	for _, port := range service.Service.Spec.Ports {
		nodePort := port.NodePort
		if nodePort == 0 {
			continue
		}
		ct.Logf("⌛ [%s] Waiting for NodePort %s:%d (%s) to become ready...",
			ct.client.ClusterName(), nodeIP, nodePort, service.Name())
		for {
			e, err := ct.client.ExecInPod(ctx,
				pod.Pod.Namespace, pod.Pod.Name, pod.Pod.Labels["name"],
				[]string{"nc", "-w", "3", "-z", nodeIP, strconv.Itoa(int(nodePort))})
			if err == nil {
				break
			}

			ct.Debugf("Error waiting for NodePort %s:%d (%s): %s: %s", nodeIP, nodePort, service.Name(), err, e.String())

			select {
			case <-ctx.Done():
				return fmt.Errorf("timeout reached waiting for NodePort %s:%d (%s) (last error: %w)", nodeIP, nodePort, service.Name(), err)
			case <-time.After(time.Second):
			}
		}
	}
	return nil
}

func (ct *ConnectivityTest) waitForCiliumEndpoint(ctx context.Context, client *k8s.Client, namespace, name string) error {
	ct.Logf("⌛ [%s] Waiting for CiliumEndpoint for pod %s/%s to appear...", client.ClusterName(), namespace, name)
	for {
		_, err := client.GetCiliumEndpoint(ctx, ct.params.TestNamespace, name, metav1.GetOptions{})
		if err == nil {
			return nil
		}

		ct.Debugf("[%s] Error getting CiliumEndpoint for pod %s/%s: %s", client.ClusterName(), namespace, name, err)

		select {
		case <-ctx.Done():
			return fmt.Errorf("aborted waiting for CiliumEndpoint for pod %s to appear: %w (last error: %s)", name, ctx.Err(), err)
		case <-time.After(2 * time.Second):
			continue
		}
	}
}
