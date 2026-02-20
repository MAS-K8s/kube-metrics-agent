package dto

type DeploymentStatus struct {
	Name                string `json:"name"`
	Namespace           string `json:"namespace"`
	DesiredReplicas     int32  `json:"desiredReplicas"`
	CurrentReplicas     int32  `json:"currentReplicas"`
	AvailableReplicas   int32  `json:"availableReplicas"`
	UnavailableReplicas int32  `json:"unavailableReplicas"`
	UpdatedReplicas     int32  `json:"updatedReplicas"`
	ReadyReplicas       int32  `json:"readyReplicas"`
}

// IsHealthy returns true if the deployment has reached the desired replicas
// and has no unavailable replicas.
func (ds DeploymentStatus) IsHealthy() bool {
	return ds.DesiredReplicas == ds.AvailableReplicas &&
		ds.UnavailableReplicas == 0
}

// IsScaling returns true if the deployment is not yet fully at desired state.
func (ds DeploymentStatus) IsScaling() bool {
	return ds.CurrentReplicas != ds.DesiredReplicas ||
		ds.UpdatedReplicas != ds.DesiredReplicas
}