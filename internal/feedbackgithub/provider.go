package feedbackgithub

import (
	"context"

	"github.com/SofiaFlux/summa42/internal/fieldfeedback"
)

func NewSink(cfg Config) (fieldfeedback.Sink, error) {
	client, err := newClient(cfg)
	if err != nil {
		return nil, err
	}
	return githubSink{client: client}, nil
}

type githubSink struct{ client *client }

func (s githubSink) Create(ctx context.Context, payload fieldfeedback.IssuePayload) (string,int64,error) {
	return s.client.createIssue(ctx,payload)
}
func (s githubSink) FindByMarker(ctx context.Context, marker string) (string,bool,error) {
	return s.client.findByMarker(ctx,marker)
}
