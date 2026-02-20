package k8s

import (
	"context"
	"fmt"
	"go-controller-agent/dto"
	"sync"
	"time"

	"github.com/go-logr/logr"
	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// Scaler handles Kubernetes Deployment scaling operations.
type Scaler struct {
	clientset *kubernetes.Clientset
	logger    logr.Logger

	// Lock is only needed to avoid concurrent Update races on the same deployment.
	// Reads are safe without locking.
	mu sync.Mutex
}

func NewScaler(clientset *kubernetes.Clientset, logger logr.Logger) *Scaler {
	return &Scaler{
		clientset: clientset,
		logger:    logger,
	}
}

// ScaleDeployment scales a deployment to the desired replica count.
// If the deployment is already at desired replicas, it does nothing.
func (s *Scaler) ScaleDeployment(
	ctx context.Context,
	namespace string,
	name string,
	desiredReplicas int32,
) error {
	// Read current state (no lock needed for GET)
	deployment, err := s.GetDeployment(ctx, namespace, name)
	if err != nil {
		return err
	}

	currentReplicas := derefInt32(deployment.Spec.Replicas)

	if currentReplicas == desiredReplicas {
		s.logger.V(1).Info("No scaling needed",
			"namespace", namespace,
			"deployment", name,
			"current", currentReplicas,
			"desired", desiredReplicas,
		)
		return nil
	}

	// Lock only for the Update path to avoid concurrent writers
	s.mu.Lock()
	defer s.mu.Unlock()

	// Re-fetch latest before updating (avoids updating stale resourceVersion)
	deployment, err = s.clientset.AppsV1().Deployments(namespace).Get(
		ctx,
		name,
		metav1.GetOptions{},
	)
	if err != nil {
		return fmt.Errorf("get deployment for update: %w", err)
	}

	deployment.Spec.Replicas = &desiredReplicas

	updated, err := s.clientset.AppsV1().Deployments(namespace).Update(
		ctx,
		deployment,
		metav1.UpdateOptions{},
	)
	if err != nil {
		return fmt.Errorf("update deployment replicas: %w", err)
	}

	got := derefInt32(updated.Spec.Replicas)
	if got != desiredReplicas {
		return fmt.Errorf("replica verification failed: expected %d, got %d", desiredReplicas, got)
	}

	s.logger.Info("✅ Deployment scaled successfully",
		"namespace", namespace,
		"deployment", name,
		"from", currentReplicas,
		"to", desiredReplicas,
	)

	return nil
}

// GetCurrentReplicas returns the desired replicas configured in spec (not status).
func (s *Scaler) GetCurrentReplicas(
	ctx context.Context,
	namespace string,
	name string,
) (int32, error) {
	deployment, err := s.GetDeployment(ctx, namespace, name)
	if err != nil {
		return 0, err
	}
	return derefInt32(deployment.Spec.Replicas), nil
}

// WaitForScale waits until AvailableReplicas equals desiredReplicas or timeout occurs.
func (s *Scaler) WaitForScale(
	ctx context.Context,
	namespace string,
	name string,
	desiredReplicas int32,
	timeout time.Duration,
) error {
	deadline := time.Now().Add(timeout)

	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for {
		if time.Now().After(deadline) {
			return fmt.Errorf("timeout waiting for deployment to scale: %s/%s to %d",
				namespace, name, desiredReplicas)
		}

		deployment, err := s.GetDeployment(ctx, namespace, name)
		if err != nil {
			return err
		}

		if deployment.Status.AvailableReplicas == desiredReplicas {
			s.logger.Info("✅ Deployment reached desired state",
				"namespace", namespace,
				"deployment", name,
				"replicas", desiredReplicas,
			)
			return nil
		}

		s.logger.V(1).Info("Waiting for deployment to scale",
			"namespace", namespace,
			"deployment", name,
			"available", deployment.Status.AvailableReplicas,
			"desired", desiredReplicas,
		)

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			// continue
		}
	}
}

// GetDeployment retrieves the Kubernetes Deployment resource.
func (s *Scaler) GetDeployment(
	ctx context.Context,
	namespace string,
	name string,
) (*appsv1.Deployment, error) {
	deployment, err := s.clientset.AppsV1().Deployments(namespace).Get(
		ctx,
		name,
		metav1.GetOptions{},
	)
	if err != nil {
		return nil, fmt.Errorf("get deployment %s/%s: %w", namespace, name, err)
	}
	return deployment, nil
}

// GetDeploymentStatus returns a DTO containing detailed deployment status.
func (s *Scaler) GetDeploymentStatus(
	ctx context.Context,
	namespace string,
	name string,
) (*dto.DeploymentStatus, error) {
	deployment, err := s.GetDeployment(ctx, namespace, name)
	if err != nil {
		return nil, err
	}

	status := &dto.DeploymentStatus{
		Name:                deployment.Name,
		Namespace:           deployment.Namespace,
		DesiredReplicas:     derefInt32(deployment.Spec.Replicas),
		CurrentReplicas:     deployment.Status.Replicas,
		AvailableReplicas:   deployment.Status.AvailableReplicas,
		UnavailableReplicas: deployment.Status.UnavailableReplicas,
		UpdatedReplicas:     deployment.Status.UpdatedReplicas,
		ReadyReplicas:       deployment.Status.ReadyReplicas,
	}

	return status, nil
}

func derefInt32(v *int32) int32 {
	if v == nil {
		return 0
	}
	return *v
}