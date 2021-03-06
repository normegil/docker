// Tools to init easily a temporary docker container, waiting for the service inside the container to start correctly.
// To create the container, see the New() function.
package docker

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/google/uuid"
	"io"
	"net"
	"strconv"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	docker "github.com/docker/docker/client"
	"github.com/docker/go-connections/nat"
	"github.com/normegil/connectionutils"
	"github.com/normegil/interval"
	"github.com/pkg/errors"
)

const dockerAddress string = "127.0.0.1"
const maxWaitTime = 5 * time.Second
const stepWaitTime = 10 * time.Millisecond

// Options gather the needed data to create the container.
type Options struct {
	// Name of the container.
	Name string
	// Image is the container image name.
	Image string
	// PortBinding is a collection of port binding needed to access the container.
	Ports []PortBinding
	// EnvironmentVariables define the variables inside the container
	EnvironmentVariables map[string]string
	// If specified, this logger will be used to log messages during initialisation of the docker (And at closing/removing time).
	Logger Logger
}

// PortBinding should follow this structure.
type PortBinding struct {
	// Protocol can be TCP,UDP,...
	Protocol string
	// Internal port to bind to.
	Internal int
	// ExternalInterval define the range of possible external port that can be mapped to the specified internal port.
	ExternalInterval string
}

// ContainerInfo return the container info needed to connect and to use the underlying service.
type ContainerInfo struct {
	// Container ID
	Identifier string
	// Address is the address of the container.
	Address net.IP
	// Ports will return the selected external ports, associated to PortBindings specified as Inputs at the creation of the container.
	Ports map[PortBinding]int
}

// Create a new container. The function will return some infos on the created container and a function to call to close and remove the container.
func New(options Options) (*ContainerInfo, func() error, error) {
	var l Logger = &defaultLogger{}
	if nil != options.Logger {
		l = options.Logger
	}

	l.Printf("New docker client from environment")
	client, err := docker.NewEnvClient()
	if nil != err {
		return nil, nil, errors.Wrap(err, "MongoDB: Could not create docker client")
	}

	if err = pullImage(client, options); err != nil {
		return nil, nil, errors.Wrap(err, "Downloading image: "+options.Image)
	}

	ip := net.ParseIP(dockerAddress)
	if err := checkOptions(options); err != nil {
		return nil, nil, errors.New("Docker instance cannot be used without a external port")
	}

	suffix, err := uuid.NewRandom()
	if nil != err {
		return nil, nil, errors.Wrapf(err, "generating docker suffix for %s", options.Name)
	}

	containerName := options.Name + "-" + suffix.String()
	dockerPorts, err := selectPorts(ip, options.Ports)
	if err != nil {
		return nil, nil, errors.Wrap(err, "Selecting ports")
	}
	portBindings := toDockerPortBindings(ip, dockerPorts)
	l.Printf("Port Bindings: %+v", portBindings)

	varDefinitions := make([]string, 0)
	for key, value := range options.EnvironmentVariables {
		varDefinitions = append(varDefinitions, key+"="+value)
	}

	l.Printf("Creating container: %+v", containerName)
	ctx := context.Background()
	containerInfo, err := client.ContainerCreate(ctx, &container.Config{
		Image:        options.Image,
		ExposedPorts: toExposedPorts(options.Ports),
		Env:          varDefinitions,
	}, &container.HostConfig{
		PortBindings: portBindings,
	}, nil, containerName)
	if nil != err {
		return nil, nil, errors.Wrap(err, "Could not create container ("+containerName+")")
	}
	for _, warning := range containerInfo.Warnings {
		l.Printf(warning)
	}

	l.Printf("Starting container: " + containerName)
	containerID := containerInfo.ID
	if err := client.ContainerStart(ctx, containerID, types.ContainerStartOptions{}); nil != err {
		return nil, nil, errors.Wrap(err, "Could not start container ("+containerName+")")
	}

	l.Printf("Waiting for container: " + containerName)
	reachablePorts := dockerPorts[options.Ports[0]]
	if err := waitContainer(client, containerID, dockerAddress+":"+strconv.Itoa(reachablePorts), maxWaitTime); nil != err {
		return nil, nil, errors.Wrap(err, "Container not started withing time limit")
	}
	l.Printf("Container started: " + containerName)

	return &ContainerInfo{
			Identifier: containerID,
			Address:    ip,
			Ports:      dockerPorts,
		}, func() error {
			l.Printf("Removing container: " + containerName)
			ctx := context.Background()
			if err := client.ContainerRemove(ctx, containerID, types.ContainerRemoveOptions{Force: true}); nil != err {
				return errors.Wrap(err, "MongoDB: Could not remove "+containerName)
			}
			return nil
		}, nil
}

func pullImage(client *docker.Client, options Options) error {
	var l Logger = &defaultLogger{}
	if nil != options.Logger {
		l = options.Logger
	}

	l.Printf("Listing available images")
	images, err := client.ImageList(context.Background(), types.ImageListOptions{})
	if err != nil {
		return errors.Wrap(err, "Listing images")
	}
	for _, image := range images {
		l.Printf("Available: %s (Searched:%s)", image.RepoTags, options.Image)
		for _, tag := range image.RepoTags {
			if tag == options.Image {
				return nil
			}
		}
	}

	l.Printf("Pulling %s", options.Image)
	events, err := client.ImagePull(context.Background(), options.Image, types.ImagePullOptions{})
	if err != nil {
		return errors.Wrap(err, "Pulling image: "+options.Image)
	}

	stream := json.NewDecoder(events)

	type Event struct {
		Status         string `json:"status"`
		Error          string `json:"error"`
		Progress       string `json:"progress"`
		ProgressDetail struct {
			Current int `json:"current"`
			Total   int `json:"total"`
		} `json:"progressDetail"`
	}
	var event Event

	for {
		if err := stream.Decode(&event); nil != err {
			if io.EOF == err {
				break
			}

			return errors.Wrapf(err, "Pulling %s (Error decoding json stream)", options.Image)
		}
	}
	l.Printf("Image %s pulled", options.Image)
	return nil
}

func checkOptions(options Options) error {
	if nil == options.Ports || 0 == len(options.Ports) {
		return errors.New("At least one port should be open for external communication")
	}
	return nil
}

func toExposedPorts(ports []PortBinding) nat.PortSet {
	exposed := make(map[nat.Port]struct{})
	for _, binding := range ports {
		exposed[nat.Port(strconv.Itoa(binding.Internal)+"/"+binding.Protocol)] = struct{}{}
	}
	return nat.PortSet(exposed)
}

func selectPorts(address net.IP, possiblePorts []PortBinding) (map[PortBinding]int, error) {
	used := make([]int, 0)
	toReturn := make(map[PortBinding]int)
	for _, binding := range possiblePorts {
		interval, err := interval.ParseIntervalInteger(binding.ExternalInterval)
		if err != nil {
			return nil, errors.Wrapf(err, "Parsing %s", binding.ExternalInterval)
		}
		selected := connectionutils.SelectPortExcluding(address, *interval, used)
		toReturn[binding] = selected.Port
	}
	return toReturn, nil
}

func toDockerPortBindings(address net.IP, ports map[PortBinding]int) map[nat.Port][]nat.PortBinding {
	toReturn := make(map[nat.Port][]nat.PortBinding)
	for binding, selectedPort := range ports {
		toReturn[nat.Port(strconv.Itoa(binding.Internal)+"/"+binding.Protocol)] = []nat.PortBinding{
			{
				//HostIP:   "0.0.0.0",
				HostPort: strconv.Itoa(selectedPort), // + "/" + binding.Protocol,
			},
		}
	}
	return toReturn
}

func waitContainer(client *docker.Client, containerID string, hostport string, maxWait time.Duration) error {
	if err := waitStarted(client, containerID, maxWait); nil != err {
		return err
	}
	if err := waitReachable(hostport, maxWait); nil != err {
		return err
	}
	return nil
}

func waitReachable(hostport string, maxWait time.Duration) error {
	done := time.Now().Add(maxWait)
	for time.Now().Before(done) {
		c, err := net.Dial("tcp", hostport)
		if nil == err {
			return c.Close()
		}
		time.Sleep(stepWaitTime)
	}
	return fmt.Errorf("Could not reach %s {WaitingTime: %+v}", hostport, maxWait)
}

func waitStarted(client *docker.Client, containerID string, maxWait time.Duration) error {
	done := time.Now().Add(maxWait)
	for time.Now().Before(done) {
		ctx := context.Background()
		c, err := client.ContainerInspect(ctx, containerID)
		if err != nil {
			break
		}
		if c.State.Running {
			return nil
		}
		time.Sleep(stepWaitTime)
	}
	return fmt.Errorf("Container not started: %s {WaitingTime: %+v}", containerID, maxWait)
}
