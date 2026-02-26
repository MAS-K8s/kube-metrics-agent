package k8s

import (
	"context"
	"fmt"
	"time"

	"go-controller-agent/dto"

	"github.com/go-logr/logr"
	appsv1 "k8s.io/api/apps/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
)

type Scaler struct {
	clientset *kubernetes.Clientset
	logger    logr.Logger
}


func NewScaler(clientset *kubernetes.Clientset, logger logr.Logger) *Scaler {
	return &Scaler{clientset: clientset, logger: logger}
}

// ✅ Allows main.go to pass clientset into collector
func (s *Scaler) Clientset() *kubernetes.Clientset {
	return s.clientset
}

// ListDeploymentsBySelector:
// - if namespace == "" => list across ALL namespaces
// - else list within one namespace
func (s *Scaler) ListDeploymentsBySelector(ctx context.Context, namespace, selector string) ([]appsv1.Deployment, error) {
	ns := namespace
	// client-go convention: empty namespace means "all namespaces" for List.
	list, err := s.clientset.AppsV1().Deployments(ns).List(ctx, metav1.ListOptions{
		LabelSelector: selector,
	})
	if err != nil {
		return nil, fmt.Errorf("list deployments selector=%q namespace=%q: %w", selector, namespace, err)
	}
	return list.Items, nil
}

func (s *Scaler) GetDeployment(ctx context.Context, namespace, name string) (*appsv1.Deployment, error) {
	dep, err := s.clientset.AppsV1().Deployments(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("get deployment %s/%s: %w", namespace, name, err)
	}
	return dep, nil
}

func PodLabelSelectorFromDeployment(dep *appsv1.Deployment) (string, error) {
	if dep == nil || dep.Spec.Selector == nil {
		return "", fmt.Errorf("deployment selector is nil")
	}
	ls := labels.Set(dep.Spec.Selector.MatchLabels).String()
	if ls == "" {
		return "", fmt.Errorf("deployment selector matchLabels empty")
	}
	return ls, nil
}

func (s *Scaler) GetDeploymentStatus(ctx context.Context, namespace, name string) (*dto.DeploymentStatus, error) {
	dep, err := s.GetDeployment(ctx, namespace, name)
	if err != nil {
		return nil, err
	}

	status := &dto.DeploymentStatus{
		Name:                dep.Name,
		Namespace:           dep.Namespace,
		CurrentReplicas:     dep.Status.Replicas,
		AvailableReplicas:   dep.Status.AvailableReplicas,
		UnavailableReplicas: dep.Status.UnavailableReplicas,
		UpdatedReplicas:     dep.Status.UpdatedReplicas,
		ReadyReplicas:       dep.Status.ReadyReplicas,
	}

	if dep.Spec.Replicas != nil {
		status.DesiredReplicas = *dep.Spec.Replicas
	}
	return status, nil
}

func (s *Scaler) ScaleDeployment(ctx context.Context, namespace, name string, desired int32) error {
	backoff := wait.Backoff{
		Steps:    4,
		Duration: 150 * time.Millisecond,
		Factor:   1.8,
		Jitter:   0.1,
	}

	var lastErr error
	err := wait.ExponentialBackoffWithContext(ctx, backoff, func(ctx context.Context) (bool, error) {
		dep, err := s.clientset.AppsV1().Deployments(namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			if apierrors.IsNotFound(err) {
				return false, err
			}
			lastErr = err
			return false, nil
		}

		cur := int32(0)
		if dep.Spec.Replicas != nil {
			cur = *dep.Spec.Replicas
		}
		if cur == desired {
			return true, nil
		}

		dep.Spec.Replicas = &desired
		_, err = s.clientset.AppsV1().Deployments(namespace).Update(ctx, dep, metav1.UpdateOptions{})
		if err != nil {
			lastErr = err
			return false, nil
		}

		s.logger.Info("✅ Scaled deployment", "namespace", namespace, "deployment", name, "from", cur, "to", desired)
		return true, nil
	})

	if err != nil {
		if lastErr != nil {
			return fmt.Errorf("scale deployment %s/%s to %d failed: %w", namespace, name, desired, lastErr)
		}
		return fmt.Errorf("scale deployment %s/%s to %d failed: %w", namespace, name, desired, err)
	}
	return nil
}