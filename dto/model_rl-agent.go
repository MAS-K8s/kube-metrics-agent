package dto

type RLRequest struct {
	DeploymentName string  `json:"deployment_name"`
	Namespace      string  `json:"namespace"`
	Metrics        Metrics `json:"metrics"`
	TrainingMode   bool    `json:"training_mode"`
	Timestamp      int64   `json:"timestamp"`
}

type RLResponse struct {
	Action              int       `json:"action"`
	ActionName          string    `json:"action_name"`
	Confidence          float64   `json:"confidence"`
	Reward              float64   `json:"reward,omitempty"`
	Epsilon             float64   `json:"epsilon,omitempty"`
	QValues             []float64 `json:"q_values,omitempty"`
	ValueEstimate       float64   `json:"value_estimate,omitempty"`
	ActionProbabilities []float64 `json:"action_probabilities,omitempty"`
	Success             bool      `json:"success"`
	Message             string    `json:"message,omitempty"`
	BufferSize          int       `json:"buffer_size,omitempty"`
	TrainingSteps       int       `json:"training_steps,omitempty"`
}
