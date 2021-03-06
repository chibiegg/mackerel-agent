package command

import (
	"crypto/sha1"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"time"

	"github.com/Songmu/retry"
	"github.com/mackerelio/mackerel-agent/agent"
	"github.com/mackerelio/mackerel-agent/checks"
	"github.com/mackerelio/mackerel-agent/config"
	"github.com/mackerelio/mackerel-agent/logging"
	"github.com/mackerelio/mackerel-agent/mackerel"
	"github.com/mackerelio/mackerel-agent/metrics"
	"github.com/mackerelio/mackerel-agent/spec"
	"github.com/mackerelio/mackerel-agent/util"
)

var logger = logging.GetLogger("command")
var metricsInterval = 60 * time.Second

var retryNum uint = 20
var retryInterval = 3 * time.Second

// prepareHost collects specs of the host and sends them to Mackerel server.
// A unique host-id is returned by the server if one is not specified.
func prepareHost(conf *config.Config, api *mackerel.API) (*mackerel.Host, error) {
	// XXX this configuration should be moved to under spec/linux
	os.Setenv("PATH", "/sbin:/usr/sbin:/bin:/usr/bin:"+os.Getenv("PATH"))
	os.Setenv("LANG", "C") // prevent changing outputs of some command, e.g. ifconfig.

	doRetry := func(f func() error) {
		retry.Retry(retryNum, retryInterval, f)
	}

	filterErrorForRetry := func(err error) error {
		if err != nil {
			logger.Warningf("%s", err.Error())
		}
		if apiErr, ok := err.(*mackerel.Error); ok && apiErr.IsClientError() {
			// don't retry when client error (APIKey error etc.) occurred
			return nil
		}
		return err
	}

	hostname, meta, interfaces, customIdentifier, lastErr := collectHostSpecs()
	if lastErr != nil {
		return nil, fmt.Errorf("error while collecting host specs: %s", lastErr.Error())
	}

	var result *mackerel.Host
	if hostID, err := conf.LoadHostID(); err != nil { // create

		if customIdentifier != "" {
			retry.Retry(3, 2*time.Second, func() error {
				result, lastErr = api.FindHostByCustomIdentifier(customIdentifier)
				return filterErrorForRetry(lastErr)
			})
			if result != nil {
				hostID = result.ID
			}
		}

		if result == nil {
			logger.Debugf("Registering new host on mackerel...")

			doRetry(func() error {
				hostID, lastErr = api.CreateHost(mackerel.HostSpec{
					Name:             hostname,
					Meta:             meta,
					Interfaces:       interfaces,
					RoleFullnames:    conf.Roles,
					DisplayName:      conf.DisplayName,
					CustomIdentifier: customIdentifier,
				})
				return filterErrorForRetry(lastErr)
			})

			if lastErr != nil {
				return nil, fmt.Errorf("Failed to register this host: %s", lastErr.Error())
			}

			doRetry(func() error {
				result, lastErr = api.FindHost(hostID)
				return filterErrorForRetry(lastErr)
			})
			if lastErr != nil {
				return nil, fmt.Errorf("Failed to find this host on mackerel: %s", lastErr.Error())
			}
		}
	} else { // check the hostID is valid or not
		doRetry(func() error {
			result, lastErr = api.FindHost(hostID)
			return filterErrorForRetry(lastErr)
		})
		if lastErr != nil {
			if fsStorage, ok := conf.HostIDStorage.(*config.FileSystemHostIDStorage); ok {
				return nil, fmt.Errorf("Failed to find this host on mackerel (You may want to delete file \"%s\" to register this host to an another organization): %s", fsStorage.HostIDFile(), lastErr.Error())
			}
			return nil, fmt.Errorf("Failed to find this host on mackerel: %s", lastErr.Error())
		}
	}

	hostSt := conf.HostStatus.OnStart
	if hostSt != "" && hostSt != result.Status {
		doRetry(func() error {
			lastErr = api.UpdateHostStatus(result.ID, hostSt)
			return filterErrorForRetry(lastErr)
		})
		if lastErr != nil {
			return nil, fmt.Errorf("Failed to set default host status: %s, %s", hostSt, lastErr.Error())
		}
	}

	lastErr = conf.SaveHostID(result.ID)
	if lastErr != nil {
		return nil, fmt.Errorf("Failed to save host ID: %s", lastErr.Error())
	}

	return result, nil
}

// prepareCustomIdentiferHosts collects the host information based on the
// configuration of the custom_identifier fields.
func prepareCustomIdentiferHosts(conf *config.Config, api *mackerel.API) map[string]*mackerel.Host {
	customIdentifierHosts := make(map[string]*mackerel.Host)
	customIdentifiers := make(map[string]bool) // use a map to make them unique
	for _, pluginConfigs := range conf.Plugin {
		for _, pluginConfig := range pluginConfigs {
			if pluginConfig.CustomIdentifier != nil {
				customIdentifiers[*pluginConfig.CustomIdentifier] = true
			}
		}
	}
	for customIdentifier := range customIdentifiers {
		host, err := api.FindHostByCustomIdentifier(customIdentifier)
		if err != nil {
			logger.Warningf("Failed to retrieve the host of custom_identifier: %s, %s", customIdentifier, err)
			continue
		}
		customIdentifierHosts[customIdentifier] = host
	}
	return customIdentifierHosts
}

// Interval between each updating host specs.
var specsUpdateInterval = 1 * time.Hour

func delayByHost(host *mackerel.Host) int {
	s := sha1.Sum([]byte(host.ID))
	return int(s[len(s)-1]) % int(config.PostMetricsInterval.Seconds())
}

// Context context object
type Context struct {
	Agent                 *agent.Agent
	Config                *config.Config
	Host                  *mackerel.Host
	API                   *mackerel.API
	CustomIdentifierHosts map[string]*mackerel.Host
}

type postValue struct {
	values   []*mackerel.CreatingMetricsValue
	retryCnt int
}

func newPostValue(values []*mackerel.CreatingMetricsValue) *postValue {
	return &postValue{values, 0}
}

type loopState uint8

const (
	loopStateFirst loopState = iota
	loopStateDefault
	loopStateQueued
	loopStateHadError
	loopStateTerminating
)

func loop(c *Context, termCh chan struct{}) error {
	quit := make(chan struct{})
	defer close(quit) // broadcast terminating

	// Periodically update host specs.
	go updateHostSpecsLoop(c, quit)

	postQueue := make(chan *postValue, c.Config.Connection.PostMetricsBufferSize)
	go enqueueLoop(c, postQueue, quit)

	postDelaySeconds := delayByHost(c.Host)
	initialDelay := postDelaySeconds / 2
	logger.Debugf("wait %d seconds before initial posting.", initialDelay)
	select {
	case <-termCh:
		return nil
	case <-time.After(time.Duration(initialDelay) * time.Second):
		c.Agent.InitPluginGenerators(c.API)
	}

	termCheckerCh := make(chan struct{})
	termMetricsCh := make(chan struct{})

	// fan-out termCh
	go func() {
		for range termCh {
			termCheckerCh <- struct{}{}
			termMetricsCh <- struct{}{}
		}
	}()

	runCheckersLoop(c, termCheckerCh, quit)

	lState := loopStateFirst
	for {
		select {
		case <-termMetricsCh:
			if lState == loopStateTerminating {
				return fmt.Errorf("received terminate instruction again. force return")
			}
			lState = loopStateTerminating
			if len(postQueue) <= 0 {
				return nil
			}
		case v := <-postQueue:
			origPostValues := [](*postValue){v}
			if len(postQueue) > 0 {
				// Bulk posting. However at most "two" metrics are to be posted, so postQueue isn't always empty yet.
				logger.Debugf("Merging datapoints with next queued ones")
				nextValues := <-postQueue
				origPostValues = append(origPostValues, nextValues)
			}

			delaySeconds := 0
			switch lState {
			case loopStateFirst: // request immediately to create graph defs of host
				// nop
			case loopStateQueued:
				delaySeconds = c.Config.Connection.PostMetricsDequeueDelaySeconds
			case loopStateHadError:
				// TODO: better interval calculation. exponential backoff or so.
				delaySeconds = c.Config.Connection.PostMetricsRetryDelaySeconds
			case loopStateTerminating:
				// dequeue and post every one second when terminating.
				delaySeconds = 1
			default:
				// Sending data at every 0 second from all hosts causes request flooding.
				// To prevent flooding, this loop sleeps for some seconds
				// which is specific to the ID of the host running agent on.
				// The sleep second is up to 60s (to be exact up to `config.Postmetricsinterval.Seconds()`.
				elapsedSeconds := int(time.Now().Unix() % int64(config.PostMetricsInterval.Seconds()))
				if postDelaySeconds > elapsedSeconds {
					delaySeconds = postDelaySeconds - elapsedSeconds
				}
			}

			// determine next loopState before sleeping
			if lState != loopStateTerminating {
				if len(postQueue) > 0 {
					lState = loopStateQueued
				} else {
					lState = loopStateDefault
				}
			}

			logger.Debugf("Sleep %d seconds before posting.", delaySeconds)
			select {
			case <-time.After(time.Duration(delaySeconds) * time.Second):
				// nop
			case <-termMetricsCh:
				if lState == loopStateTerminating {
					return fmt.Errorf("received terminate instruction again. force return")
				}
				lState = loopStateTerminating
			}

			postValues := [](*mackerel.CreatingMetricsValue){}
			for _, v := range origPostValues {
				postValues = append(postValues, v.values...)
			}
			err := c.API.PostMetricsValues(postValues)
			if err != nil {
				logger.Errorf("Failed to post metrics value (will retry): %s", err.Error())
				if lState != loopStateTerminating {
					lState = loopStateHadError
				}
				go func() {
					for _, v := range origPostValues {
						v.retryCnt++
						// It is difficult to distinguish the error is server error or data error.
						// So, if retryCnt exceeded the configured limit, postValue is considered invalid and abandoned.
						if v.retryCnt > c.Config.Connection.PostMetricsRetryMax {
							json, err := json.Marshal(v.values)
							if err != nil {
								logger.Errorf("Something wrong with post values. marshaling failed.")
							} else {
								logger.Errorf("Post values may be invalid and abandoned: %s", string(json))
							}
							continue
						}
						postQueue <- v
					}
				}()
				continue
			}
			logger.Debugf("Posting metrics succeeded.")

			if lState == loopStateTerminating && len(postQueue) <= 0 {
				return nil
			}
		}
	}
}

func updateHostSpecsLoop(c *Context, quit chan struct{}) {
	for {
		c.UpdateHostSpecs()
		select {
		case <-quit:
			return
		case <-time.After(specsUpdateInterval):
			// nop
		}
	}
}

func enqueueLoop(c *Context, postQueue chan *postValue, quit chan struct{}) {
	metricsResult := c.Agent.Watch()
	for {
		select {
		case <-quit:
			return
		case result := <-metricsResult:
			created := float64(result.Created.Unix())
			creatingValues := [](*mackerel.CreatingMetricsValue){}
			for _, values := range result.Values {
				hostID := c.Host.ID
				if values.CustomIdentifier != nil {
					if host, ok := c.CustomIdentifierHosts[*values.CustomIdentifier]; ok {
						hostID = host.ID
					} else {
						continue
					}
				}
				for name, value := range (map[string]float64)(values.Values) {
					if math.IsNaN(value) || math.IsInf(value, 0) {
						logger.Warningf("Invalid value: hostID = %s, name = %s, value = %f\n is not sent.", hostID, name, value)
						continue
					}

					creatingValues = append(
						creatingValues,
						&mackerel.CreatingMetricsValue{
							HostID: hostID,
							Name:   name,
							Time:   created,
							Value:  value,
						},
					)
				}
			}
			logger.Debugf("Enqueuing task to post metrics.")
			postQueue <- newPostValue(creatingValues)
		}
	}
}

// runCheckersLoop generates "checker" goroutines
// which run for each checker commands and one for HTTP POSTing
// the reports to Mackerel API.
func runCheckersLoop(c *Context, termCheckerCh <-chan struct{}, quit <-chan struct{}) {
	var (
		checkReportCh          chan *checks.Report
		reportCheckImmediateCh chan struct{}
	)
	for _, checker := range c.Agent.Checkers {
		if checkReportCh == nil {
			checkReportCh = make(chan *checks.Report)
			reportCheckImmediateCh = make(chan struct{})
		}

		go func(checker checks.Checker) {
			var (
				lastStatus  = checks.StatusUndefined
				lastMessage = ""
			)

			util.Periodically(
				func() {
					report, err := checker.Check()
					if err != nil {
						logger.Errorf("checker %v: %s", checker, err)
						return
					}

					logger.Debugf("checker %q: report=%v", checker.Name, report)

					if report.Status == checks.StatusOK && report.Status == lastStatus && report.Message == lastMessage {
						// Do not report if nothing has changed
						return
					}

					checkReportCh <- report

					// If status has changed, send it immediately
					// but if the status was OK and it's first invocation of a check, do not
					if report.Status != lastStatus && !(report.Status == checks.StatusOK && lastStatus == checks.StatusUndefined) {
						logger.Debugf("checker %q: status has changed %v -> %v: send it immediately", checker.Name, lastStatus, report.Status)
						reportCheckImmediateCh <- struct{}{}
					}

					lastStatus = report.Status
					lastMessage = report.Message
				},
				checker.Interval(),
				quit,
			)
		}(checker)
	}
	if checkReportCh != nil {
		go func() {
			exit := false
			for !exit {
				select {
				case <-time.After(1 * time.Minute):
				case <-termCheckerCh:
					logger.Debugf("received 'term' chan")
					exit = true
				case <-reportCheckImmediateCh:
					logger.Debugf("received 'immediate' chan")
				}

				reports := []*checks.Report{}
			DrainCheckReport:
				for {
					select {
					case report := <-checkReportCh:
						reports = append(reports, report)
					default:
						break DrainCheckReport
					}
				}

				for i, report := range reports {
					logger.Debugf("reports[%d]: %#v", i, report)
				}

				if len(reports) == 0 {
					continue
				}

				err := c.API.ReportCheckMonitors(c.Host.ID, reports)
				if err != nil {
					logger.Errorf("ReportCheckMonitors: %s", err)

					// queue back the reports
					go func() {
						for _, report := range reports {
							logger.Debugf("queue back report: %#v", report)
							checkReportCh <- report
						}
					}()
				}
			}
		}()
	} else {
		// consume termCheckerCh
		go func() {
			for range termCheckerCh {
			}
		}()
	}
}

// collectHostSpecs collects host specs (correspond to "name", "meta", "interfaces" and "customIdentifier" fields in API v0)
func collectHostSpecs() (string, map[string]interface{}, []spec.NetInterface, string, error) {
	hostname, err := os.Hostname()
	if err != nil {
		return "", nil, nil, "", fmt.Errorf("failed to obtain hostname: %s", err.Error())
	}

	specGens := specGenerators()
	cGen := spec.SuggestCloudGenerator()
	if cGen != nil {
		specGens = append(specGens, cGen)
	}
	meta := spec.Collect(specGens)

	var customIdentifier string
	if cGen != nil {
		customIdentifier, err = cGen.SuggestCustomIdentifier()
		if err != nil {
			logger.Warningf("Error while suggesting custom identifier. err: %s", err.Error())
		}
	}

	interfaces, err := interfaceGenerator().Generate()
	if err != nil {
		return "", nil, nil, "", fmt.Errorf("failed to collect interfaces: %s", err.Error())
	}
	return hostname, meta, interfaces, customIdentifier, nil
}

// UpdateHostSpecs updates the host information that is already registered on Mackerel.
func (c *Context) UpdateHostSpecs() {
	logger.Debugf("Updating host specs...")

	hostname, meta, interfaces, customIdentifier, err := collectHostSpecs()
	if err != nil {
		logger.Errorf("While collecting host specs: %s", err)
		return
	}

	err = c.API.UpdateHost(c.Host.ID, mackerel.HostSpec{
		Name:             hostname,
		Meta:             meta,
		Interfaces:       interfaces,
		RoleFullnames:    c.Config.Roles,
		Checks:           c.Config.CheckNames(),
		DisplayName:      c.Config.DisplayName,
		CustomIdentifier: customIdentifier,
	})

	if err != nil {
		logger.Errorf("Error while updating host specs: %s", err)
	} else {
		logger.Debugf("Host specs sent.")
	}
}

// Prepare sets up API and registers the host data to the Mackerel server.
// Use returned values to call Run().
func Prepare(conf *config.Config) (*Context, error) {
	api, err := mackerel.NewAPI(conf.Apibase, conf.Apikey, conf.Verbose)
	if err != nil {
		return nil, fmt.Errorf("Failed to prepare an api: %s", err.Error())
	}

	host, err := prepareHost(conf, api)
	if err != nil {
		return nil, fmt.Errorf("Failed to prepare host: %s", err.Error())
	}

	return &Context{
		Agent:  NewAgent(conf),
		Config: conf,
		Host:   host,
		API:    api,
		CustomIdentifierHosts: prepareCustomIdentiferHosts(conf, api),
	}, nil
}

// RunOnce collects specs and metrics, then output them to stdout.
func RunOnce(conf *config.Config) error {
	graphdefs, hostSpec, metrics, err := runOncePayload(conf)
	if err != nil {
		return err
	}

	json, err := json.Marshal(map[string]interface{}{
		"host":    hostSpec,
		"metrics": metrics,
	})
	if err != nil {
		logger.Warningf("Error while marshaling graphdefs: err = %s, graphdefs = %s.", err.Error(), graphdefs)
		return err
	}
	fmt.Println(string(json))
	return nil
}

func runOncePayload(conf *config.Config) ([]mackerel.CreateGraphDefsPayload, *mackerel.HostSpec, *agent.MetricsResult, error) {
	hostname, meta, interfaces, customIdentifier, err := collectHostSpecs()
	if err != nil {
		logger.Errorf("While collecting host specs: %s", err)
		return nil, nil, nil, err
	}

	origInterval := metricsInterval
	metricsInterval = 1 * time.Second
	defer func() {
		metricsInterval = origInterval
	}()
	ag := NewAgent(conf)
	graphdefs := ag.CollectGraphDefsOfPlugins()
	metrics := ag.CollectMetrics(time.Now())
	return graphdefs, &mackerel.HostSpec{
		Name:             hostname,
		Meta:             meta,
		Interfaces:       interfaces,
		RoleFullnames:    conf.Roles,
		Checks:           conf.CheckNames(),
		DisplayName:      conf.DisplayName,
		CustomIdentifier: customIdentifier,
	}, metrics, nil
}

// NewAgent creates a new instance of agent.Agent from its configuration conf.
func NewAgent(conf *config.Config) *agent.Agent {
	return &agent.Agent{
		MetricsGenerators: prepareGenerators(conf),
		PluginGenerators:  pluginGenerators(conf),
		Checkers:          createCheckers(conf),
	}
}

// Run starts the main metric collecting logic and this function will never return.
func Run(c *Context, termCh chan struct{}) error {
	logger.Infof("Start: apibase = %s, hostName = %s, hostID = %s", c.Config.Apibase, c.Host.Name, c.Host.ID)

	err := loop(c, termCh)
	if err == nil && c.Config.HostStatus.OnStop != "" {
		// TODO error handling. support retire(?)
		e := c.API.UpdateHostStatus(c.Host.ID, c.Config.HostStatus.OnStop)
		if e != nil {
			logger.Errorf("Failed update host status on stop: %s", e)
		}
	}
	return err
}

func createCheckers(conf *config.Config) []checks.Checker {
	checkers := []checks.Checker{}

	for name, pluginConfig := range conf.Plugin["checks"] {
		checker := checks.Checker{
			Name:   name,
			Config: pluginConfig,
		}
		logger.Debugf("Checker created: %v", checker)
		checkers = append(checkers, checker)
	}

	return checkers
}

func prepareGenerators(conf *config.Config) []metrics.Generator {
	diagnostic := conf.Diagnostic
	generators := metricsGenerators(conf)
	if diagnostic {
		generators = append(generators, &metrics.AgentGenerator{})
	}
	return generators
}
