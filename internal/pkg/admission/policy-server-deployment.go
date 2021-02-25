package admission

import (
	"context"
	"fmt"
	"path/filepath"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/chimera-kube/chimera-controller/internal/pkg/constants"
)

// reconcilePolicyServerDeployment reconciles the Deployment that runs the PolicyServer
// component
func (r *AdmissionReconciler) reconcilePolicyServerDeployment(ctx context.Context) error {
	configMapVersion, err := r.policyServerConfigMapVersion(ctx)
	if err != nil {
		return fmt.Errorf("Cannot get policy-server ConfigMap version: %v", err)
	}
	r.Log.Info("reconcilePolicyServerDeployment", "Current config version", configMapVersion)

	err = r.Client.Create(ctx, r.deployment(configMapVersion))
	if err == nil {
		return nil
	}
	if !apierrors.IsAlreadyExists(err) {
		return err
	}

	return r.updatePolicyServerDeployment(ctx, configMapVersion)
}

// isPolicyServerReady returns true when the PolicyServer deployment is running only
// fresh replicas that are reflecting its Spec.
// This works using the same code of `kubectl rollout status <deployment>`
func (r *AdmissionReconciler) isPolicyServerReady(ctx context.Context) (bool, error) {
	deployment := &appsv1.Deployment{}
	err := r.Client.Get(ctx, client.ObjectKey{
		Namespace: r.DeploymentsNamespace,
		Name:      constants.PolicyServerDeploymentName,
	}, deployment)
	if err != nil {
		return false, fmt.Errorf("Cannot retrieve existing policy-server Deployment: %v", err)
	}

	// This code takes inspiration from how `kubectl rollout status deployment <name>`
	// works. The source code can be found here:
	// https://github.com/kubernetes/kubectl/blob/ddb56dde55b6b8de6eba1efbd1d435bed7b40ff4/pkg/polymorphichelpers/rollout_status.go#L75-L91
	if deployment.Generation <= deployment.Status.ObservedGeneration {
		cond := getProgressingDeploymentCondition(deployment.Status)
		if cond != nil && cond.Reason == "ProgressDeadlineExceeded" {
			return false, fmt.Errorf("deployment %q exceeded its progress deadline", deployment.Name)
		}
		if deployment.Spec.Replicas != nil && deployment.Status.UpdatedReplicas < *deployment.Spec.Replicas {
			return false,
				&PolicyServerNotReadyError{
					Message: fmt.Sprintf("Waiting for deployment %q rollout to finish: %d out of %d new replicas have been updated",
						deployment.Name, deployment.Status.UpdatedReplicas, *deployment.Spec.Replicas)}
		}
		if deployment.Status.Replicas > deployment.Status.UpdatedReplicas {
			return false, &PolicyServerNotReadyError{
				Message: fmt.Sprintf("Waiting for deployment %q rollout to finish: %d old replicas are pending termination",
					deployment.Name, deployment.Status.Replicas-deployment.Status.UpdatedReplicas)}
		}
		if deployment.Status.AvailableReplicas < deployment.Status.UpdatedReplicas {
			return false, &PolicyServerNotReadyError{
				Message: fmt.Sprintf("Waiting for deployment %q rollout to finish: %d of %d updated replicas are available",
					deployment.Name, deployment.Status.AvailableReplicas, deployment.Status.UpdatedReplicas)}
		}
		// deployment successfully rolled out
		return true, nil
	}
	return false, &PolicyServerNotReadyError{
		Message: fmt.Sprintf("Waiting for deployment spec update to be observed")}
}

// GetDeploymentCondition returns the condition with the provided type.
func getProgressingDeploymentCondition(status appsv1.DeploymentStatus) *appsv1.DeploymentCondition {
	for i := range status.Conditions {
		c := status.Conditions[i]
		if c.Type == appsv1.DeploymentProgressing {
			return &c
		}
	}
	return nil
}

func (r *AdmissionReconciler) updatePolicyServerDeployment(ctx context.Context, configMapVersion string) error {
	deployment := &appsv1.Deployment{}
	err := r.Client.Get(ctx, client.ObjectKey{
		Namespace: r.DeploymentsNamespace,
		Name:      constants.PolicyServerDeploymentName,
	}, deployment)
	if err != nil {
		return fmt.Errorf("Cannot retrieve existing policy-server Deployment: %v", err)
	}
	currentConfigVersion, found := deployment.Spec.Template.ObjectMeta.Annotations[constants.PolicyServerDeploymentConfigAnnotation]
	if !found || currentConfigVersion != configMapVersion {
		// the current deployment is using an older version of the configuration
		patch := createPatch(configMapVersion)
		err = r.Client.Patch(ctx, deployment, client.RawPatch(types.StrategicMergePatchType, patch))
		if err != nil {
			return fmt.Errorf("Cannot patch policy-server Deployment: %v", err)
		}
		r.Log.Info("deployment patched")
	}
	return nil
}

func createPatch(configMapVersion string) []byte {
	patch := fmt.Sprintf(`
{
	"apiVersion": "apps/v1",
	"kind": "Deployment",
	"spec": {
		"template": {
			"metadata": {
				"annotations": {
					"kubectl.kubernetes.io/restartedAt": "%s",
					"%s": "%s"
				}
			}
		}
	}
}`, time.Now().Format(time.RFC3339),
		constants.PolicyServerDeploymentConfigAnnotation, configMapVersion)
	return []byte(patch)
}

func (r *AdmissionReconciler) deployment(configMapVersion string) *appsv1.Deployment {
	var webhookReplicas int32 = 1

	const (
		certsVolumeName             = "certs"
		policiesConfigContainerPath = "/config"
		policiesFilename            = "policies.yml"
		policiesVolumeName          = "policies"
		secretsContainerPath        = "/pki"
	)

	admissionContainer := corev1.Container{
		Name:  constants.PolicyServerDeploymentName,
		Image: constants.AdmissionImage,
		VolumeMounts: []corev1.VolumeMount{
			{
				Name:      certsVolumeName,
				ReadOnly:  true,
				MountPath: secretsContainerPath,
			},
			{
				Name:      policiesVolumeName,
				ReadOnly:  true,
				MountPath: policiesConfigContainerPath,
			},
		},
		Env: []corev1.EnvVar{
			{
				Name:  "CHIMERA_CERT_FILE",
				Value: filepath.Join(secretsContainerPath, "admission-cert"),
			},
			{
				Name:  "CHIMERA_KEY_FILE",
				Value: filepath.Join(secretsContainerPath, "admission-key"),
			},
			{
				Name:  "CHIMERA_PORT",
				Value: fmt.Sprintf("%d", constants.PolicyServerPort),
			},
			{
				Name:  "CHIMERA_POLICIES_DOWNLOAD_DIR",
				Value: "/tmp/",
			},
			{
				Name:  "CHIMERA_POLICIES",
				Value: filepath.Join(policiesConfigContainerPath, policiesFilename),
			},
		},
		ReadinessProbe: &corev1.Probe{
			Handler: corev1.Handler{
				HTTPGet: &corev1.HTTPGetAction{
					Path:   constants.PolicyServerReadinessProbe,
					Port:   intstr.FromInt(constants.PolicyServerPort),
					Scheme: corev1.URISchemeHTTPS,
				},
			},
		},
	}

	templateAnnotations := map[string]string{
		constants.PolicyServerDeploymentConfigAnnotation: configMapVersion,
	}

	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      constants.PolicyServerDeploymentName,
			Namespace: r.DeploymentsNamespace,
			Labels:    constants.AdmissionLabels,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &webhookReplicas,
			Selector: &metav1.LabelSelector{
				MatchLabels: constants.AdmissionLabels,
			},
			Strategy: appsv1.DeploymentStrategy{
				Type: appsv1.RollingUpdateDeploymentStrategyType,
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels:      constants.AdmissionLabels,
					Annotations: templateAnnotations,
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{admissionContainer},
					Volumes: []corev1.Volume{
						{
							Name: certsVolumeName,
							VolumeSource: corev1.VolumeSource{
								Secret: &corev1.SecretVolumeSource{
									SecretName: constants.PolicyServerSecretName,
								},
							},
						},
						{
							Name: policiesVolumeName,
							VolumeSource: corev1.VolumeSource{
								ConfigMap: &corev1.ConfigMapVolumeSource{
									LocalObjectReference: corev1.LocalObjectReference{
										Name: constants.PolicyServerConfigMapName,
									},
									Items: []corev1.KeyToPath{
										{
											Key:  constants.PolicyServerConfigPoliciesEntry,
											Path: policiesFilename,
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
