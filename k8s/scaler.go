package k8s

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/go-logr/logr"
	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// Scaler handles Kubernetes deployment scaling
type Scaler struct {
	clientset *kubernetes.Clientset
	logger    logr.Logger
	mu        sync.Mutex
}

// NewScaler creates a new scaler
func NewScaler(clientset *kubernetes.Clientset, logger logr.Logger) *Scaler {
	return &Scaler{
		clientset: clientset,
		logger:    logger,
	}
}

// ScaleDeployment scales a deployment to the desired replica count
func (s *Scaler) ScaleDeployment(
	ctx context.Context,
	namespace string,
	name string,
	desiredReplicas int32,
) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Get current deployment
	deployment, err := s.clientset.AppsV1().Deployments(namespace).Get(
		ctx,
		name,
		metav1.GetOptions{},
	)
	if err != nil {
		return fmt.Errorf("failed to get deployment: %w", err)
	}

	currentReplicas := int32(0)
	if deployment.Spec.Replicas != nil {
		currentReplicas = *deployment.Spec.Replicas
	}

	// Check if scaling is needed
	if currentReplicas == desiredReplicas {
		s.logger.V(1).Info("No scaling needed",
			"deployment", name,
			"current", currentReplicas,
			"desired", desiredReplicas)
		return nil
	}

	// Update replica count
	deployment.Spec.Replicas = &desiredReplicas

	// Apply update
	updated, err := s.clientset.AppsV1().Deployments(namespace).Update(
		ctx,
		deployment,
		metav1.UpdateOptions{},
	)
	if err != nil {
		return fmt.Errorf("failed to update deployment: %w", err)
	}

	// Verify update
	if updated.Spec.Replicas == nil || *updated.Spec.Replicas != desiredReplicas {
		return fmt.Errorf("verification failed: expected %d, got %v",
			desiredReplicas, updated.Spec.Replicas)
	}

	s.logger.Info("✅ Deployment scaled successfully",
		"deployment", name,
		"from", currentReplicas,
		"to", desiredReplicas)

	return nil
}

// GetCurrentReplicas gets the current replica count
func (s *Scaler) GetCurrentReplicas(
	ctx context.Context,
	namespace string,
	name string,
) (int32, error) {
	deployment, err := s.clientset.AppsV1().Deployments(namespace).Get(
		ctx,
		name,
		metav1.GetOptions{},
	)
	if err != nil {
		return 0, fmt.Errorf("failed to get deployment: %w", err)
	}

	if deployment.Spec.Replicas == nil {
		return 0, nil
	}

	return *deployment.Spec.Replicas, nil
}

// WaitForScale waits for deployment to reach desired replica count
func (s *Scaler) WaitForScale(
	ctx context.Context,
	namespace string,
	name string,
	desiredReplicas int32,
	timeout time.Duration,
) error {
	deadline := time.Now().Add(timeout)

	for time.Now().Before(deadline) {
		deployment, err := s.clientset.AppsV1().Deployments(namespace).Get(
			ctx,
			name,
			metav1.GetOptions{},
		)
		if err != nil {
			return fmt.Errorf("failed to get deployment: %w", err)
		}

		// Check available replicas
		if deployment.Status.AvailableReplicas == desiredReplicas {
			s.logger.Info("✅ Deployment reached desired state",
				"deployment", name,
				"replicas", desiredReplicas)
			return nil
		}

		s.logger.V(1).Info("Waiting for deployment to scale",
			"deployment", name,
			"current", deployment.Status.AvailableReplicas,
			"desired", desiredReplicas)

		// Wait before checking again
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
			// Continue loop
		}
	}

	return fmt.Errorf("timeout waiting for deployment to scale")
}

// GetDeployment retrieves a deployment
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
		return nil, fmt.Errorf("failed to get deployment: %w", err)
	}

	return deployment, nil
}

// GetDeploymentStatus returns detailed status information
func (s *Scaler) GetDeploymentStatus(
	ctx context.Context,
	namespace string,
	name string,
) (*DeploymentStatus, error) {
	deployment, err := s.GetDeployment(ctx, namespace, name)
	if err != nil {
		return nil, err
	}

	status := &DeploymentStatus{
		Name:               deployment.Name,
		Namespace:          deployment.Namespace,
		DesiredReplicas:    int32(0),
		CurrentReplicas:    deployment.Status.Replicas,
		AvailableReplicas:  deployment.Status.AvailableReplicas,
		UnavailableReplicas: deployment.Status.UnavailableReplicas,
		UpdatedReplicas:    deployment.Status.UpdatedReplicas,
		ReadyReplicas:      deployment.Status.ReadyReplicas,
	}

	if deployment.Spec.Replicas != nil {
		status.DesiredReplicas = *deployment.Spec.Replicas
	}

	return status, nil
}

// DeploymentStatus contains deployment status information
type DeploymentStatus struct {
	Name                string
	Namespace           string
	DesiredReplicas     int32
	CurrentReplicas     int32
	AvailableReplicas   int32
	UnavailableReplicas int32
	UpdatedReplicas     int32
	ReadyReplicas       int32
}

// IsHealthy checks if deployment is healthy
func (ds *DeploymentStatus) IsHealthy() bool {
	return ds.DesiredReplicas == ds.AvailableReplicas &&
		ds.UnavailableReplicas == 0
}

// IsScaling checks if deployment is currently scaling
func (ds *DeploymentStatus) IsScaling() bool {
	return ds.CurrentReplicas != ds.DesiredReplicas ||
		ds.UpdatedReplicas != ds.DesiredReplicas
}