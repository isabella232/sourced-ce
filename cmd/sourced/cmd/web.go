package cmd

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/pkg/browser"
	"github.com/pkg/errors"
	"github.com/src-d/sourced-ce/cmd/sourced/compose"
	"github.com/src-d/sourced-ce/cmd/sourced/compose/workdir"
	"github.com/src-d/sourced-ce/cmd/sourced/dir"
)

// The service name used in docker-compose.yml for the srcd/sourced-ui image
const containerName = "sourced-ui"

type webCmd struct {
	Command `name:"web" short-description:"Open the web interface in your browser." long-description:"Open the web interface in your browser, by default at: http://127.0.0.1:8088 user:admin pass:admin"`
}

func (c *webCmd) Execute(args []string) error {
	return OpenUI(2 * time.Second)
}

func init() {
	rootCmd.AddCommand(&webCmd{})
}

func openUI(address string) error {
	// docker-compose returns 0.0.0.0 which is correct for the bind address
	// but incorrect as connect address
	url := fmt.Sprintf("http://%s", strings.Replace(address, "0.0.0.0", "127.0.0.1", 1))

	for {
		client := http.Client{Timeout: time.Second}
		if _, err := client.Get(url); err == nil {
			break
		}

		time.Sleep(1 * time.Second)
	}

	if err := browser.OpenURL(url); err != nil {
		return errors.Wrap(err, "could not open the browser")
	}

	return nil
}

func checkFailFast(stdout *bytes.Buffer) (bool, error) {
	err := compose.RunWithIO(context.Background(),
		os.Stdin, stdout, nil, "port", containerName, "8088")
	if workdir.ErrMalformed.Is(err) || dir.ErrNotExist.Is(err) || dir.ErrNotValid.Is(err) {
		return true, err
	}

	if err != nil {
		return false, err
	}

	return false, nil
}

func waitForContainer(stdout *bytes.Buffer) {
	for {
		if err := compose.RunWithIO(context.Background(),
			os.Stdin, stdout, nil, "port", containerName, "8088"); err == nil {
			break
		}

		time.Sleep(1 * time.Second)
	}
}

var stateExtractor = regexp.MustCompile(`(?m)^srcd-[\w\d]+.*(Up|Exit (\w+))`)

// runMonitor checks the status of the containers in order to early exit in case
// an unrecoverable error occurs.
// The monitoring is performed by running `docker-compose ps <service>` for each
// service returned by `docker-compose config --services`, and by grepping the
// state from the stdout using a regex.
// Getting the state of all the containers in a single pass by running `docker-compose ps`
// and by using a multi-line regex to extract both service name and state is not reliable.
// The reason is that the prefix of a container can be very long, especially for local
// initialization, due to the value that we set for `COMPOSE_PROJECT_NAME` env var, and
// docker-compose may split the name into multiple lines.
// E.g.:
//
// Name                                                       Command                       State                                     Ports
// ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------
// srcd-l1vzzxjzl3nln2vudhlzztdlbi9qcm9qzwn0cy8uz28td29ya3nwywnll3nyyy9naxrodwiuy29tl3nln   /bin/bblfsh-web -addr :808 ...   Up                      0.0.0.0:9999->8080/tcp
// 2vudhlzztdlbg_bblfsh-web_1
func runMonitor(ch chan<- error) {
	runMonitorService := func(service string, ch chan<- error) {
		for {
			var stdout bytes.Buffer
			if err := compose.RunWithIO(context.Background(),
				os.Stdin, &stdout, nil, "ps", service); err != nil {
				ch <- errors.Wrapf(err, "cannot get status service %s", service)
				return
			}

			matches := stateExtractor.FindAllStringSubmatch(
				strings.TrimSpace(stdout.String()), -1)
			for _, match := range matches {
				state := match[1]

				if strings.HasPrefix(state, "Exit") {
					if service != "ghsync" && service != "gitcollector" {
						ch <- fmt.Errorf("service '%s' is in state '%s'", service, state)
						return
					}

					returnCode := state[len("Exit "):len(state)]
					if returnCode != "0" {
						ch <- fmt.Errorf("service '%s' exited with return code: %s",
							service, returnCode)
						return
					}

					continue
				}

				if state != "Up" {
					ch <- fmt.Errorf("service '%s' is in state '%s'", service, state)
					return
				}

			}

			time.Sleep(1 * time.Second)
		}
	}

	go func() {
		var servicesBuf bytes.Buffer
		if err := compose.RunWithIO(context.Background(),
			os.Stdin, &servicesBuf, nil, "config", "--services"); err != nil {
			ch <- errors.Wrap(err, "cannot get list of services")
			return
		}

		services := strings.Split(strings.TrimSpace(servicesBuf.String()), "\n")

		for _, service := range services {
			go runMonitorService(service, ch)
		}
	}()
}

// OpenUI opens the browser with the UI.
func OpenUI(timeout time.Duration) error {
	var stdout bytes.Buffer
	failFast, err := checkFailFast(&stdout)
	if failFast {
		return err
	}

	ch := make(chan error)
	containerReady := err == nil

	runMonitor(ch)

	go func() {
		if !containerReady {
			waitForContainer(&stdout)
		}

		address := strings.TrimSpace(stdout.String())
		if address == "" {
			ch <- fmt.Errorf("could not find the public port of %s", containerName)
			return
		}

		ch <- openUI(address)
	}()

	fmt.Println(`
Once source{d} is fully initialized, the UI will be available, by default at:
  http://127.0.0.1:8088
  user:admin
  pass:admin
	`)

	if timeout > 5*time.Second {
		stopSpinner := startSpinner("Initializing source{d}...")
		defer stopSpinner()
	}

	select {
	case err := <-ch:
		return err
	case <-time.After(timeout):
		return fmt.Errorf("error opening the UI, the container is not running after %v", timeout)
	}
}

type spinner struct {
	msg      string
	charset  []int
	interval time.Duration

	stop chan bool
}

func startSpinner(msg string) func() {
	charset := []int{'⠋', '⠙', '⠹', '⠸', '⠼', '⠴', '⠦', '⠧', '⠇', '⠏'}
	if runtime.GOOS == "windows" {
		charset = []int{'|', '/', '-', '\\'}
	}

	s := &spinner{
		msg:      msg,
		charset:  charset,
		interval: 200 * time.Millisecond,
		stop:     make(chan bool),
	}
	s.Start()

	return s.Stop
}

func (s *spinner) Start() {
	go s.printLoop()
}

func (s *spinner) Stop() {
	s.stop <- true
}

func (s *spinner) printLoop() {
	i := 0
	for {
		select {
		case <-s.stop:
			fmt.Println(s.msg)
			return
		default:
			char := string(s.charset[i%len(s.charset)])
			if runtime.GOOS == "windows" {
				fmt.Printf("\r%s %s", s.msg, char)
			} else {
				fmt.Printf("%s %s\n\033[A", s.msg, char)
			}

			time.Sleep(s.interval)
		}

		i++
		if len(s.charset) == i {
			i = 0
		}
	}
}
