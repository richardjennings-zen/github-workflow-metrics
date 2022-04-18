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
	ws, err := workflowStats(ctx, c, conf.githubOwner, conf.githubRepo, conf.githubRunId)
	if err != nil {
		log.Fatal(err)
	}
	if err := pushMetrics(ctx, ws, conf.remoteWriteUrl, conf.remoteWriteUsername, conf.remoteWritePassword); err != nil {
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
	StepStats         struct {
		Name  string
		Start time.Time
		End   time.Time
	}
	JobStats struct {
		Name  string
		Start time.Time
		End   time.Time
		Steps []StepStats
	}
	WorkflowStats struct {
		Id    int64
		Name  string
		Start time.Time
		End   time.Time
		Jobs  []JobStats
		T     int
	}
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

func workflowStats(ctx context.Context, client *github.Client, owner string, repo string, runId int64) (WorkflowStats, error) {
	ws := WorkflowStats{}
	workflow, _, err := client.Actions.GetWorkflowRunByID(ctx, owner, repo, runId)
	if err != nil {
		return ws, err
	}
	ws.Id = runId
	ws.Start = workflow.GetCreatedAt().Time
	ws.End = workflow.GetUpdatedAt().Time
	ws.T++
	ws.Name = workflow.GetName()
	jobs, _, err := client.Actions.ListWorkflowJobs(ctx, owner, repo, runId, &github.ListWorkflowJobsOptions{})
	for _, job := range jobs.Jobs {
		ws.T++
		j := JobStats{}
		j.Name = job.GetName()
		j.Start = job.GetStartedAt().Time
		j.End = job.GetCompletedAt().Time
		for _, step := range job.Steps {
			ws.T++
			s := StepStats{}
			s.Name = step.GetName()
			s.Start = step.GetStartedAt().Time
			s.End = step.GetCompletedAt().Time
			j.Steps = append(j.Steps, s)
		}
		ws.Jobs = append(ws.Jobs, j)
	}
	return ws, nil
}

func pushMetrics(ctx context.Context, w WorkflowStats, uri string, username string, password string) (err error) {
	var endpoint *url.URL
	if endpoint, err = url.Parse(uri); err != nil {
		return err
	}
	c, err := remote.NewWriteClient(
		"github_action_metrics",
		&remote.ClientConfig{
			URL:     &config.URL{URL: endpoint},
			Timeout: model.Duration(30 * time.Second),
			HTTPClientConfig: config.HTTPClientConfig{
				BasicAuth: &config.BasicAuth{
					Username: username,
					Password: config.Secret(password),
				},
			},
		},
	)
	writeReq := &prompb.WriteRequest{
		Timeseries: make([]prompb.TimeSeries, w.T),
	}

	// add workflow metric
	metricLabel := prompb.Label{
		Name:  "__name__",
		Value: "workflow_duration",
	}
	workflowLabel := prompb.Label{
		Name:  "workflow",
		Value: w.Name,
	}
	workflowIdLabel := prompb.Label{
		Name:  "workflow_id",
		Value: strconv.FormatInt(w.Id, 10),
	}
	writeReq.Timeseries[0].Labels = []prompb.Label{metricLabel, workflowLabel, workflowIdLabel}
	writeReq.Timeseries[0].Samples = []prompb.Sample{
		{
			Timestamp: time.Now().UnixMilli(),
			Value:     float64(w.End.Sub(w.Start).Milliseconds()),
		},
	}
	t := 1 // count of metrics processed so far
	for _, j := range w.Jobs {
		jLabel := prompb.Label{Name: "job", Value: j.Name}
		// add job metric
		writeReq.Timeseries[t].Labels = []prompb.Label{metricLabel, workflowLabel, workflowIdLabel, jLabel}
		writeReq.Timeseries[t].Samples = []prompb.Sample{
			{
				Timestamp: time.Now().UnixMilli(),
				Value:     float64(j.End.Sub(j.Start).Milliseconds()),
			},
		}
		t++
		for _, s := range j.Steps {
			sLabel := prompb.Label{Name: "step", Value: j.Name}
			// add step metrics
			writeReq.Timeseries[t].Labels = []prompb.Label{metricLabel, workflowLabel, workflowIdLabel, jLabel, sLabel}
			writeReq.Timeseries[t].Samples = []prompb.Sample{
				{
					Timestamp: time.Now().UnixMilli(),
					Value:     float64(s.End.Sub(s.Start).Milliseconds()),
				},
			}
			t++
		}
	}
	data, err := proto.Marshal(writeReq)
	if err != nil {
		return fmt.Errorf("unable to marshal protobuf: %v", err)
	}
	encoded := snappy.Encode(nil, data)
	return c.Store(ctx, encoded)
}
