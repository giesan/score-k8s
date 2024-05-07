package convert

import (
	"fmt"
	"strings"

	"github.com/pkg/errors"
	"github.com/score-spec/score-go/framework"
	scoretypes "github.com/score-spec/score-go/types"
	v1 "k8s.io/api/apps/v1"
	coreV1 "k8s.io/api/core/v1"
	machineryMeta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	"github.com/score-spec/score-k8s/internal"
	"github.com/score-spec/score-k8s/internal/project"
)

const (
	WorkloadKindDeployment    = "Deployment"
	WorkloadKindStatefulSet   = "StatefulSet"
	SelectorLabelWorkloadName = "score-workload"
)

func ConvertWorkload(state *project.State, workloadName string) ([]machineryMeta.Object, error) {
	resOutputs, err := state.GetResourceOutputForWorkload(workloadName)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to generate outputs")
	}
	sf := framework.BuildSubstitutionFunction(state.Workloads[workloadName].Spec.Metadata, resOutputs)

	spec := state.Workloads[workloadName].Spec
	manifests := make([]machineryMeta.Object, 0, 1)

	kind := WorkloadKindDeployment
	if d, ok := internal.FindAnnotation[string](spec.Metadata, internal.WorkloadKindAnnotation); ok {
		kind = d
		if kind != WorkloadKindDeployment && kind != WorkloadKindStatefulSet {
			return nil, errors.Wrapf(err, "metadata: annotations: %s: unsupported workload kind", internal.WorkloadKindAnnotation)
		}
	}

	// containers and volumes here are fun..
	// we have to collect them all based on the parent paths they get mounted in and turn these into projected volumes
	// then add the projected volumes to the deployment
	volumes := make([]coreV1.Volume, 0)
	volumeClaimTemplates := make([]coreV1.PersistentVolumeClaim, 0)

	containers := make([]coreV1.Container, 0, len(spec.Containers))
	for containerName, container := range spec.Containers {
		c := coreV1.Container{
			Name:         containerName,
			Image:        container.Image,
			Command:      container.Command,
			Args:         container.Args,
			VolumeMounts: make([]coreV1.VolumeMount, 0),
		}

		c.Resources, err = convertContainerResources(container.Resources)
		if err != nil {
			return nil, errors.Wrapf(err, "containers.%s.resources: failed to convert", containerName)
		}

		c.Env, err = convertContainerVariables(container.Variables, sf)
		if err != nil {
			return nil, errors.Wrapf(err, "containers.%s.variables: failed to convert", containerName)
		}

		containerVolumes := make([]coreV1.Volume, 0)
		containerVolumeMounts := make([]coreV1.VolumeMount, 0)

		volSubstitutionFunction := func(ref string) (string, error) {
			if parts := framework.SplitRefParts(ref); len(parts) == 2 && parts[0] == "resources" {
				resName := parts[1]
				if res, ok := spec.Resources[resName]; ok {
					return string(framework.NewResourceUid(workloadName, resName, res.Type, res.Class, res.Id)), nil
				}
				return "", fmt.Errorf("resource '%s' does not exist", resName)
			}
			return sf(ref)
		}
		for i, volume := range container.Volumes {
			if mount, vol, claim, err := convertContainerVolume(i, volume, state.Resources, volSubstitutionFunction); err != nil {
				return nil, errors.Wrapf(err, "containers.%s.volumes.%d: failed to convert", containerName, i)
			} else {
				containerVolumeMounts = append(containerVolumeMounts, mount)
				if claim != nil {
					if kind != WorkloadKindStatefulSet {
						return nil, errors.Wrapf(err, "containers.%s.volumes.%d: volume claims can only be set on stateful sets", containerName, i)
					}
					volumeClaimTemplates = append(volumeClaimTemplates, *claim)
				} else if vol != nil {
					containerVolumes = append(containerVolumes, *vol)
				}
			}
		}

		for i, f := range container.Files {
			if mount, cfg, vol, err := convertContainerFile(i, f, fmt.Sprintf("%s-%s-", workloadName, containerName), state.Workloads[workloadName].File, sf); err != nil {
				return nil, errors.Wrapf(err, "containers.%s.files.%d: failed to convert", containerName, i)
			} else {
				containerVolumeMounts = append(containerVolumeMounts, mount)
				if cfg != nil {
					manifests = append(manifests, cfg)
				}
				if vol != nil {
					containerVolumes = append(containerVolumes, *vol)
				}
			}
		}

		// collapse projected volume mounts
		containerVolumes, containerVolumeMounts, err = collapseVolumeMounts(containerVolumes, containerVolumeMounts)
		if err != nil {
			return nil, errors.Wrapf(err, "containers.%s.volumes: failed to combine projected volumes", containerName)
		}
		c.VolumeMounts = containerVolumeMounts
		volumes = append(volumes, containerVolumes...)

		if container.LivenessProbe != nil {
			c.LivenessProbe = &coreV1.Probe{ProbeHandler: buildProbe(container.LivenessProbe.HttpGet)}
		}
		if container.ReadinessProbe != nil {
			c.ReadinessProbe = &coreV1.Probe{ProbeHandler: buildProbe(container.ReadinessProbe.HttpGet)}
		}

		containers = append(containers, c)
	}

	if spec.Service != nil && len(spec.Service.Ports) > 0 {
		portList := make([]coreV1.ServicePort, 0, len(spec.Service.Ports))
		for portName, port := range spec.Service.Ports {
			var proto = coreV1.ProtocolTCP
			if port.Protocol != nil && *port.Protocol != "" {
				proto = coreV1.Protocol(strings.ToUpper(string(*port.Protocol)))
			}
			var targetPort = port.Port
			if port.TargetPort != nil && *port.TargetPort > 0 {
				targetPort = *port.TargetPort // Defaults to the published port
			}
			portList = append(portList, coreV1.ServicePort{
				Name:       portName,
				Port:       int32(port.Port),
				TargetPort: intstr.FromInt32(int32(targetPort)),
				Protocol:   proto,
			})
		}
		manifests = append(manifests, &coreV1.Service{
			TypeMeta: machineryMeta.TypeMeta{Kind: "Service", APIVersion: "v1"},
			ObjectMeta: machineryMeta.ObjectMeta{
				Name:        WorkloadServiceName(workloadName),
				Annotations: make(map[string]string),
			},
			Spec: coreV1.ServiceSpec{
				Selector: map[string]string{
					SelectorLabelWorkloadName: workloadName,
				},
				Ports: portList,
			},
		})
	}

	switch kind {
	case WorkloadKindDeployment:
		manifests = append(manifests, &v1.Deployment{
			TypeMeta: machineryMeta.TypeMeta{Kind: WorkloadKindDeployment, APIVersion: "apps/v1"},
			ObjectMeta: machineryMeta.ObjectMeta{
				Name:        workloadName,
				Annotations: make(map[string]string),
			},
			Spec: v1.DeploymentSpec{
				Replicas: internal.Ref(int32(1)),
				Selector: &machineryMeta.LabelSelector{
					MatchExpressions: []machineryMeta.LabelSelectorRequirement{
						{SelectorLabelWorkloadName, machineryMeta.LabelSelectorOpIn, []string{workloadName}},
					},
				},
				Template: coreV1.PodTemplateSpec{
					ObjectMeta: machineryMeta.ObjectMeta{
						Labels: map[string]string{
							SelectorLabelWorkloadName: workloadName,
						},
					},
					Spec: coreV1.PodSpec{
						Containers: containers,
						Volumes:    volumes,
					},
				},
			},
		})
	case WorkloadKindStatefulSet:

		// need to allocate a headless service here
		headlessServiceName := fmt.Sprintf("%s-headless-svc", workloadName)
		manifests = append(manifests, &coreV1.Service{
			TypeMeta: machineryMeta.TypeMeta{Kind: "Service", APIVersion: "v1"},
			ObjectMeta: machineryMeta.ObjectMeta{
				Name:        headlessServiceName,
				Annotations: make(map[string]string),
			},
			Spec: coreV1.ServiceSpec{
				Selector: map[string]string{
					SelectorLabelWorkloadName: workloadName,
				},
				ClusterIP: "None",
				Ports:     []coreV1.ServicePort{{Name: "default", Port: 99, TargetPort: intstr.FromInt32(99)}},
			},
		})

		manifests = append(manifests, &v1.StatefulSet{
			TypeMeta: machineryMeta.TypeMeta{Kind: WorkloadKindStatefulSet, APIVersion: "apps/v1"},
			ObjectMeta: machineryMeta.ObjectMeta{
				Name:        workloadName,
				Annotations: make(map[string]string),
			},
			Spec: v1.StatefulSetSpec{
				Replicas: internal.Ref(int32(1)),
				Selector: &machineryMeta.LabelSelector{
					MatchExpressions: []machineryMeta.LabelSelectorRequirement{
						{SelectorLabelWorkloadName, machineryMeta.LabelSelectorOpIn, []string{workloadName}},
					},
				},
				ServiceName: headlessServiceName,
				Template: coreV1.PodTemplateSpec{
					ObjectMeta: machineryMeta.ObjectMeta{
						Labels: map[string]string{
							SelectorLabelWorkloadName: workloadName,
						},
					},
					Spec: coreV1.PodSpec{
						Containers: containers,
						Volumes:    volumes,
					},
				},
				// So the puzzle here is how to get this from our volumes...
				VolumeClaimTemplates: volumeClaimTemplates,
			},
		})
	}

	return manifests, nil
}

func WorkloadServiceName(workloadName string) string {
	return fmt.Sprintf("%s-svc", workloadName)
}

func buildProbe(input scoretypes.HttpProbe) coreV1.ProbeHandler {
	ph := coreV1.ProbeHandler{
		HTTPGet: &coreV1.HTTPGetAction{
			Path:   input.Path,
			Port:   intstr.FromInt32(int32(input.Port)),
			Host:   internal.DerefOr(input.Host, ""),
			Scheme: coreV1.URIScheme(internal.DerefOr(input.Scheme, "")),
		},
	}
	if len(input.HttpHeaders) > 0 {
		h := make([]coreV1.HTTPHeader, 0, len(input.HttpHeaders))
		for _, header := range input.HttpHeaders {
			h = append(h, coreV1.HTTPHeader{Name: header.Name, Value: header.Value})
		}
		ph.HTTPGet.HTTPHeaders = h
	}
	return ph
}