package main

import (
	"context"
	"errors"
	"fmt"
	"github.com/golang/protobuf/proto"
	"github.com/golang/snappy"
	"github.com/google/go-github/v39/github"
	"github.com/prometheus/common/config"
	"github.com/prometheus/common/model"
	"github.com/prometheus/prometheus/prompb"
	"github.com/prometheus/prometheus/storage/remote"
	"golang.org/x/oauth2"
	"log"
	"net/url"
	"os"
	"strconv"
	"time"
)

func main() {
	conf, err := EnvConfigProvider{}.config()
	if err != nil {
		log.Fatal(err)
	}
	c := client(conf.githubToken)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second*60)
	defer cancel()
	ws, js, err := workflowStats(ctx, c, conf.githubOwner, conf.githubRepo, conf.githubRunId)
	if err != nil {
		log.Fatal(err)
	}
	ts := createTimeSeries(ws, js)
	if err := pushMetrics(ctx, ts, conf); err != nil {
		log.Fatal(err)
	}
}

type (
	conf struct {
		githubToken         string
		githubOwner         string
		githubRepo          string
		githubRunId         int64
		remoteWriteUrl      string
		remoteWriteUsername string
		remoteWritePassword string
	}
	ConfigProvider interface {
		config() (conf, error)
	}
	EnvConfigProvider struct{}
)

func (e EnvConfigProvider) config() (conf, error) {
	var ok bool
	var err error
	c := conf{
		githubToken: os.Getenv("GITHUB_TOKEN"),
	}
	c.githubOwner, ok = os.LookupEnv("GITHUB_OWNER")
	if !ok {
		return c, errors.New("expected GITHUB_OWNER")
	}
	c.githubRepo, ok = os.LookupEnv("GITHUB_REPO")
	if !ok {
		return c, errors.New("expected GITHUB_REPO")
	}
	runId, ok := os.LookupEnv("GITHUB_RUN_ID")
	if !ok {
		return c, errors.New("expected GITHUB_RUN_ID")
	}
	c.githubRunId, err = strconv.ParseInt(runId, 10, 64)
	if err != nil {
		return c, err
	}
	c.remoteWriteUrl, ok = os.LookupEnv("REMOTE_WRITE_URL")
	if !ok {
		return c, errors.New("expected REMOTE_WRITE_URL")
	}
	c.remoteWriteUsername, ok = os.LookupEnv("REMOTE_WRITE_USERNAME")
	if !ok {
		return c, errors.New("expected REMOTE_WRITE_USERNAME")
	}
	c.remoteWritePassword, ok = os.LookupEnv("REMOTE_WRITE_PASSWORD")
	if !ok {
		return c, errors.New("expected REMOTE_WRITE_PASSWORD")
	}
	return c, nil
}

func client(token string) *github.Client {
	if token != "" {
		httpClient := oauth2.NewClient(
			context.Background(),
			oauth2.StaticTokenSource(&oauth2.Token{AccessToken: token}),
		)
		return github.NewClient(httpClient)
	}
	return github.NewClient(nil)
}

func workflowStats(ctx context.Context, client *github.Client, owner string, repo string, runId int64) (*github.WorkflowRun, *github.Jobs, error) {
	workflow, _, err := client.Actions.GetWorkflowRunByID(ctx, owner, repo, runId)
	if err != nil {
		return workflow, nil, err
	}
	jobs, _, err := client.Actions.ListWorkflowJobs(ctx, owner, repo, runId, &github.ListWorkflowJobsOptions{})
	return workflow, jobs, err
}

func createTimeSeries(w *github.WorkflowRun, js *github.Jobs) (t []prompb.TimeSeries) {
	now := time.Now().UnixMilli()
	metricLabel := prompb.Label{Name: "__name__", Value: "workflow_duration"}
	workflowLabel := prompb.Label{Name: "workflow_name", Value: w.GetName()}
	workflowIdLabel := prompb.Label{Name: "workflow_id", Value: strconv.FormatInt(w.GetID(), 10)}
	// add workflow metric
	t = append(t, prompb.TimeSeries{
		Labels: []prompb.Label{metricLabel, workflowLabel, workflowIdLabel},
		Samples: []prompb.Sample{{
			Timestamp: now,
			Value:     w.GetUpdatedAt().Time.Sub(w.GetCreatedAt().Time).Seconds(),
		}},
	})
	for _, j := range js.Jobs {
		jLabel := prompb.Label{Name: "job_name", Value: j.GetName()}
		// add job metric
		t = append(t, prompb.TimeSeries{
			Labels: []prompb.Label{metricLabel, workflowLabel, workflowIdLabel, jLabel},
			Samples: []prompb.Sample{{
				Timestamp: now,
				Value:     j.GetCompletedAt().Time.Sub(j.GetStartedAt().Time).Seconds(),
			}},
		})
		for _, s := range j.Steps {
			sLabel := prompb.Label{Name: "step", Value: s.GetName()}
			// add step metrics
			t = append(t, prompb.TimeSeries{
				Labels: []prompb.Label{metricLabel, workflowLabel, workflowIdLabel, jLabel, sLabel},
				Samples: []prompb.Sample{{
					Timestamp: now,
					Value:     s.GetCompletedAt().Time.Sub(s.GetStartedAt().Time).Seconds(),
				}},
			})
		}
	}
	return
}

func pushMetrics(ctx context.Context, ts []prompb.TimeSeries, cnf conf) (err error) {
	var endpoint *url.URL
	if endpoint, err = url.Parse(cnf.remoteWriteUrl); err != nil {
		return err
	}
	c, err := remote.NewWriteClient(
		"github_action_metrics",
		&remote.ClientConfig{
			URL:     &config.URL{URL: endpoint},
			Timeout: model.Duration(30 * time.Second),
			HTTPClientConfig: config.HTTPClientConfig{
				BasicAuth: &config.BasicAuth{
					Username: cnf.remoteWriteUsername,
					Password: config.Secret(cnf.remoteWritePassword),
				},
			},
		},
	)
	writeReq := &prompb.WriteRequest{Timeseries: ts}
	data, err := proto.Marshal(writeReq)
	if err != nil {
		return fmt.Errorf("unable to marshal protobuf: %v", err)
	}
	encoded := snappy.Encode(nil, data)
	return c.Store(ctx, encoded)
}
