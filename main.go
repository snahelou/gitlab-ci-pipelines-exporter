package main

import (
	"flag"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/heptiolabs/healthcheck"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	gitlab "github.com/xanzy/go-gitlab"
	yaml "gopkg.in/yaml.v2"
)

type config struct {
	Gitlab struct {
		URL   string
		Token string
	}

	PollingIntervalSeconds int `yaml:"polling_interval_seconds"`
	Projects               []project
}

type project struct {
	Name string
	Ref  string
}

var (
	listenAddress = flag.String("listen-address", ":8080", "Listening address")
	configPath    = flag.String("config", "~/.gitlab-ci-pipelines-exporter.yml", "Config file path")
)

var (
	timeSinceLastRun = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "gitlab_ci_pipeline_time_since_last_run_seconds",
			Help: "Elapsed time since most recent GitLab CI pipeline run.",
		},
		[]string{"project", "ref"},
	)

	lastRunDuration = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "gitlab_ci_pipeline_last_run_duration_seconds",
			Help: "Duration of last pipeline run",
		},
		[]string{"project", "ref"},
	)
	runCount = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "gitlab_ci_pipeline_run_count",
			Help: "GitLab CI pipeline run count",
		},
		[]string{"project", "ref"},
	)

	status = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "gitlab_ci_pipeline_status",
			Help: "GitLab CI pipeline current status",
		},
		[]string{"project", "ref", "status"},
	)
)

var owned = true

func init() {
	prometheus.MustRegister(timeSinceLastRun)
	prometheus.MustRegister(lastRunDuration)
	prometheus.MustRegister(runCount)
	prometheus.MustRegister(status)
}

func getProject(p project, gc *gitlab.Client) *gitlab.Project {
	gp, _, err := gc.Projects.GetProject(p.Name)
	if err != nil {
		log.Fatalf("Unable to fetch project '%v' from the GitLab API : %v", p.Name, err.Error())
		os.Exit(1)
	}
	return gp
}

func taskOnce(p project, config *config, gc *gitlab.Client) {

	gp := getProject(p, gc)
	log.Printf("--> Polling ID: %v | %v:%v", gp.ID, p.Name, p.Ref)

	var lastPipeline *gitlab.Pipeline
	runCount.WithLabelValues(p.Name, p.Ref)

	pipelines, _, err := gc.Pipelines.ListProjectPipelines(gp.ID, &gitlab.ListProjectPipelinesOptions{Ref: gitlab.String(p.Ref)})
	if err != nil {
		log.Fatalf("Unable to get pipelines for project %s : %v", gp.Name, err.Error())
		os.Exit(1)
	}
	if len(pipelines) == 0 {
		return
	}
	if lastPipeline == nil || lastPipeline.ID != pipelines[0].ID || lastPipeline.Status != pipelines[0].Status {
		if lastPipeline != nil {
			runCount.WithLabelValues(p.Name, p.Ref).Inc()
		}

		lastPipeline, _, _ = gc.Pipelines.GetPipeline(gp.ID, pipelines[0].ID)

		lastRunDuration.WithLabelValues(p.Name, p.Ref).Set(float64(lastPipeline.Duration))

		for _, s := range []string{"success", "failed", "running"} {
			if s == lastPipeline.Status {
				status.WithLabelValues(p.Name, p.Ref, s).Set(1)
			} else {
				status.WithLabelValues(p.Name, p.Ref, s).Set(0)
			}
		}
	}

	timeSinceLastRun.WithLabelValues(p.Name, p.Ref).Set(float64(time.Since(*lastPipeline.CreatedAt).Round(time.Second).Seconds()))

}

func task(config *config, gc *gitlab.Client) {
	for {
		for _, p := range config.Projects {
			if p.Name == "*" {
				log.Printf("-> Wildcard detected: Pulling all owned projects with ref %s", p.Ref)
				projects, _, err := gc.Projects.ListProjects(&gitlab.ListProjectsOptions{Owned: &owned})
				if err != nil {
					log.Fatalf("Unable to fetch all projects from the GitLab API : %v", err.Error())
					os.Exit(1)
				}
				for _, pwild := range projects {
					go taskOnce(project{Name: pwild.PathWithNamespace, Ref: p.Ref}, config, gc)
				}
			} else {
				go taskOnce(p, config, gc)
			}
		}
		time.Sleep(time.Duration(config.PollingIntervalSeconds) * time.Second)
	}
}

func main() {
	flag.Parse()

	var config config

	configFile, err := ioutil.ReadFile(*configPath)
	if err != nil {
		log.Fatalf("Couldn't open config file : %v", err.Error())
		os.Exit(1)
	}

	err = yaml.Unmarshal(configFile, &config)
	if err != nil {
		log.Fatalf("Unable to parse config file: %v", err.Error())
		os.Exit(1)
	}

	if len(config.Projects) < 1 {
		log.Fatalf("You need to configure at least one project/ref to poll, none given")
		os.Exit(1)
	}

	log.Printf("-> Starting exporter")
	log.Printf("-> Polling %v every %vs", config.Gitlab.URL, config.PollingIntervalSeconds)
	log.Printf("-> %v project(s) configured", len(config.Projects))

	gc := gitlab.NewClient(nil, config.Gitlab.Token)
	gc.SetBaseURL(config.Gitlab.URL)

	go task(&config, gc)

	// Expose the registered metrics via HTTP.
	health := healthcheck.NewHandler()
	health.AddLivenessCheck("goroutine-threshold", healthcheck.GoroutineCountCheck(50))

	mux := http.NewServeMux()
	mux.HandleFunc("/health", health.LiveEndpoint)
	mux.Handle("/metrics", promhttp.Handler())

	log.Fatal(http.ListenAndServe(*listenAddress, mux))
}

