package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"path"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/filters"
	docker "github.com/docker/docker/client"
	"github.com/docker/docker/pkg/stdcopy"
)

type cli struct {
	client     *docker.Client
	clientNoTo *docker.Client // client without timeout for streaming requests
	logsDir    string
}

func (c *cli) captureContainerLog(ctx context.Context, id string) error {
	container, err := c.client.ContainerInspect(ctx, id)
	if err != nil {
		return fmt.Errorf("Failed to setup container log capturing: %w", err)
	}

	// TODO trim filename?
	var logName string
	containerName := strings.ReplaceAll(container.Name, "/", "__")
	if jobID, ok := container.Config.Labels["com.gitlab.gitlab-runner.job.id"]; ok {
		logName = path.Join(c.logsDir, jobID+"-"+containerName+".log")
	} else {
		logName = path.Join(c.logsDir, container.ID+"-"+containerName+".log")
	}

	logReader, err := c.clientNoTo.ContainerLogs(ctx, id,
		types.ContainerLogsOptions{ShowStdout: true, ShowStderr: true, Follow: true})
	if err != nil {
		return fmt.Errorf("Failed to setup container log capturing: %w", err)
	}
	defer logReader.Close()

	// Log file will be truncated if it already exists.
	// We rely on gitlab job id or container id to ensure that
	// we're overwriting the same log
	logFile, err := os.Create(logName)
	if err != nil {
		return fmt.Errorf("Failed to setup container log capturing: %w", err)
	}
	defer logFile.Close()

	if container.Config.Tty {
		_, err = io.Copy(logFile, logReader)
	} else {
		_, err = stdcopy.StdCopy(logFile, logFile, logReader)
	}
	if err != nil && err != io.EOF {
		return err
	}

	return nil
}

// Initialize log capturing for already running containers
func (c *cli) initCapturing(ctx context.Context, jobs *sync.Map) error {
	containers, err := c.client.ContainerList(ctx, types.ContainerListOptions{All: false})
	if err != nil {
		return err
	}

	for _, container := range containers {
		jobs.Store(container.ID, true)
		go func(id, name string) {
			log.Printf("Picking up capture of container=%s id=%s log", name, id)
			err := c.captureContainerLog(ctx, id)
			if err != nil && !errors.Is(err, context.Canceled) {
				log.Print(err)
			}
			jobs.Delete(id)
			log.Printf("Finished to capture container=%s id=%s log", name, id)
		}(container.ID, strings.Join(container.Names, " "))
	}

	return nil
}

func (c *cli) runEventLoop(ctx context.Context, jobs *sync.Map) error {
	eventsOpts := types.EventsOptions{
		Filters: filters.NewArgs(
			filters.Arg("type", "container"),
			filters.Arg("event", "start"),
		),
	}

	events, errs := c.clientNoTo.Events(ctx, eventsOpts)
	for {
		select {
		case event, ok := <-events:
			if !ok {
				log.Print("Events channel closed")
				continue
			}
			if !(event.Type == "container" && event.Action == "start") {
				log.Printf("Filtered event slipped through: %+v", event)
				continue
			}
			jobs.Store(event.Actor.ID, true)
			go func() {
				id := event.Actor.ID
				name := event.Actor.Attributes["name"]
				defer jobs.Delete(id)
				defer log.Printf("Finished to capture container=%s id=%s log", name, id)

				log.Printf("Starting to capture container=%s id=%s log", name, id)
				err := c.captureContainerLog(ctx, id)
				if err != nil && !errors.Is(err, context.Canceled) {
					log.Print(err)
				}
			}()
		case err, ok := <-errs:
			switch {
			case !ok:
				log.Print("Errors channel closed")
				return nil
			case errors.Is(err, io.EOF):
				log.Print("Event loop terminated")
				return nil
			case errors.Is(err, context.Canceled):
				return err
			default:
				return fmt.Errorf("Event loop terminated with error: %w", err)
			}
		}
	}

	return nil
}

func (c *cli) run(ctx context.Context) error {
	var jobs sync.Map
	if err := c.initCapturing(ctx, &jobs); err != nil {
		return err
	}

	for {
		log.Print("Starting event loop")
		err := c.runEventLoop(ctx, &jobs)
		if errors.Is(err, context.Canceled) {
			break
		} else if err != nil {
			log.Print(err)
			continue
		}
	}

	for {
		select {
		case <-time.After(time.Millisecond * 250):
			empty := true
			jobs.Range(func(key, value any) bool {
				empty = false
				return empty
			})
			if empty {
				return nil
			}
		case <-time.After(time.Second * 10):
			return errors.New("Gracefull shutdown timed out")

		}
	}

	return nil
}

func createLogsDir(dir string) (func() error, error) {
	err := os.Mkdir(dir, 0755)
	if os.IsExist(err) {
		return func() error { return nil }, nil
	} else if err != nil {
		return nil, fmt.Errorf("Failed to create directory for logs: %w", err)
	}

	selfDestruct := func() error {
		// os.Remove deletes only empty directories
		return os.Remove(dir)
	}

	return selfDestruct, nil
}

func main() {
	var (
		printVersion  = flag.Bool("v", false, "print version")
		logsDir       = flag.String("d", "container_log_capture", "directory where to save logs")
		clientTimeout = flag.Duration("docker.timeout", time.Second*10, "docker client request timeout")
	)
	flag.Parse()

	if *printVersion {
		buildInfo, _ := debug.ReadBuildInfo()
		fmt.Print(buildInfo)
		os.Exit(0)
	}

	logsDirSelfDestruct, err := createLogsDir(*logsDir)
	if err != nil {
		log.Fatalf("Failed to create directory for logs: %s", err)
	}
	defer logsDirSelfDestruct()

	client, err := docker.NewClientWithOpts(docker.FromEnv,
		docker.WithAPIVersionNegotiation(), docker.WithTimeout(*clientTimeout))
	if err != nil {
		log.Fatalf("Failed to create docker client: %s", err)
	}

	clientNoTo, err := docker.NewClientWithOpts(docker.FromEnv,
		docker.WithAPIVersionNegotiation())
	if err != nil {
		log.Fatalf("Failed to create docker client: %s", err)
	}

	dockerInfo, err := client.Ping(context.Background())
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("Docker info: %+v", dockerInfo)

	cli := cli{client, clientNoTo, *logsDir}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigint := make(chan os.Signal, 1)
	signal.Notify(sigint, os.Interrupt)
	go func() {
		<-sigint
		log.Print("Received SIGING. Shuting down")
		cancel()
	}()

	if err := cli.run(ctx); err != nil {
		log.Fatalf("Finished with error: %s", err)
	}
	log.Print("Finished")
}