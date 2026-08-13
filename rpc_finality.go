package ethertest

import "context"

func (api *finalityAPI) PauseFinality(ctx context.Context) (bool, error) {
	err := api.node.PauseFinality(ctx)
	return err == nil, err
}

func (api *finalityAPI) ResumeFinality(ctx context.Context) (bool, error) {
	err := api.node.ResumeFinality(ctx)
	return err == nil, err
}

func (api *finalityAPI) FinalityStatus() FinalityStatus {
	return api.node.FinalityStatus()
}
