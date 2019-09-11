package main

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"time"

	"github.com/gorilla/websocket"
	keptnevents "github.com/keptn/go-utils/pkg/events"
	keptnmodels "github.com/keptn/go-utils/pkg/models"
	keptnutils "github.com/keptn/go-utils/pkg/utils"

	"github.com/cloudevents/sdk-go/pkg/cloudevents"
	"github.com/cloudevents/sdk-go/pkg/cloudevents/client"
	cloudeventshttp "github.com/cloudevents/sdk-go/pkg/cloudevents/transport/http"
	"github.com/cloudevents/sdk-go/pkg/cloudevents/types"
	"github.com/google/uuid"
	"github.com/kelseyhightower/envconfig"

	"gopkg.in/yaml.v2"
)

const timeout = 60
const configservice = "CONFIGURATION_SERVICE"
const eventbroker = "EVENTBROKER"
const api = "API"

type envConfig struct {
	Port int    `envconfig:"RCV_PORT" default:"8080"`
	Path string `envconfig:"RCV_PATH" default:"/"`
}

type doneEventData struct {
	Result  string `json:"result"`
	Message string `json:"message"`
	Version string `json:"version"`
}

type Client struct {
	httpClient *http.Client
}

// ResourceListBody parameter
// swagger:model ResourceListBody
type ResourceListBody struct {

	// resources
	Resources []*keptnmodels.Resource `json:"resources"`
}

func main() {
	var env envConfig
	if err := envconfig.Process("", &env); err != nil {
		log.Fatalf("failed to process env var: %s", err)
	}
	os.Exit(_main(os.Args[1:], env))
}

func _main(args []string, env envConfig) int {

	ctx := context.Background()

	t, err := cloudeventshttp.New(
		cloudeventshttp.WithPort(env.Port),
		cloudeventshttp.WithPath(env.Path),
	)
	if err != nil {
		log.Fatalf("failed to create transport: %v", err)
	}

	c, err := client.New(t)
	if err != nil {
		log.Fatalf("failed to create client: %v", err)
	}
	log.Fatalf("failed to start receiver: %s", c.StartReceiver(ctx, gotEvent))

	return 0
}

func newClient() *Client {
	client := Client{
		httpClient: &http.Client{
			Timeout: 5 * time.Second,
		},
	}
	return &client
}

func gotEvent(ctx context.Context, event cloudevents.Event) error {
	var shkeptncontext string
	event.Context.ExtensionAs("shkeptncontext", &shkeptncontext)

	logger := keptnutils.NewLogger(shkeptncontext, event.Context.GetID(), "shipyard-service")

	// open websocket connection to api component
	endPoint, err := getServiceEndpoint(api)
	if err != nil {
		return err
	}

	if endPoint.Host == "" {
		const errorMsg = "Host of api not set"
		logger.Error(errorMsg)
		return errors.New(errorMsg)
	}

	connData := &keptnutils.ConnectionData{}
	if err := event.DataAs(connData); err != nil {
		logger.Error(fmt.Sprintf("Data of the event is incompatible. %s", err.Error()))
		return err
	}

	ws, _, err := keptnutils.OpenWS(*connData, endPoint)
	if err != nil {
		logger.Error(fmt.Sprintf("Opening websocket connection failed. %s", err.Error()))
		return err
	}
	defer ws.Close()

	//if event.Type() == "sh.keptn.internal.events.project.create" { // for keptn internal topics
	if event.Type() == keptnevents.InternalProjectCreateEventType {
		version, err := createProjectAndProcessShipyard(event, *logger, ws)
		if err := logErrAndRespondWithDoneEvent(event, version, err, *logger, ws); err != nil {
			return err
		}

		return nil
	}

	const errorMsg = "Received unexpected keptn event that cannot be processed"
	if err := keptnutils.WriteWSLog(ws, createEventCopy(event, "sh.keptn.events.log"), errorMsg, true, "INFO"); err != nil {
		logger.Error(fmt.Sprintf("Could not write log to websocket. %s", err.Error()))
	}
	logger.Error(errorMsg)
	return errors.New(errorMsg)
}

// createProjectAndProcessShipyard creates a project and stages defined in the shipyard
func createProjectAndProcessShipyard(event cloudevents.Event, logger keptnutils.Logger, ws *websocket.Conn) (*keptnmodels.Version, error) {
	eventData := keptnevents.ProjectCreateEventData{}
	if err := event.DataAs(&eventData); err != nil {
		return nil, err
	}

	client := newClient()
	// create project
	project := keptnmodels.Project{
		ProjectName: eventData.Project,
	}
	if err := client.createProject(project, logger); err != nil {
		return nil, fmt.Errorf("Creating project %s failed. %s", project.ProjectName, err.Error())
	}
	if err := keptnutils.WriteWSLog(ws, createEventCopy(event, "sh.keptn.events.log"), fmt.Sprintf("Project %s created", project.ProjectName), false, "INFO"); err != nil {
		logger.Error(fmt.Sprintf("Could not write log to websocket. %s", err.Error()))
	}

	shipyard := keptnmodels.Shipyard{}
	data, err := base64.StdEncoding.DecodeString(eventData.Shipyard)
	if err != nil {
		logger.Error(fmt.Sprintf("Could not decode shipyard. %s", err.Error()))
		return nil, err
	}
	err = yaml.Unmarshal(data, &shipyard)
	if err != nil {
		logger.Error(fmt.Sprintf("Could not unmarshal shipyard. %s", err.Error()))
		return nil, err
	}

	// process shipyard file and create stages
	for _, shipyardStage := range shipyard.Stages {
		if err := client.createStage(project.ProjectName, shipyardStage.Name, logger); err != nil {
			return nil, fmt.Errorf("Creating stage %s failed. %s", shipyardStage.Name, err.Error())
		}
		if err := keptnutils.WriteWSLog(ws, createEventCopy(event, "sh.keptn.events.log"), fmt.Sprintf("Stage %s created", shipyardStage.Name), false, "INFO"); err != nil {
			logger.Error(fmt.Sprintf("Could not write log to websocket. %s", err.Error()))
		}
	}

	// store shipyard.yaml
	return storeResourceForProject(project.ProjectName, string(data), logger)
}

// storeResourceForProject stores the resource for a project using the keptnutils.ResourceHandler
func storeResourceForProject(projectName, shipyard string, logger keptnutils.Logger) (*keptnmodels.Version, error) {
	configServiceURL, err := getServiceEndpoint(configservice)
	if err != nil {
		logger.Error(fmt.Sprintf("Could not get service endpoint for %s: %s", configservice, err.Error()))
		return nil, err
	}
	handler := keptnutils.NewResourceHandler(configServiceURL.String())
	uri := "shipyard.yaml"
	resource := keptnmodels.Resource{ResourceURI: &uri, ResourceContent: shipyard}
	versionStr, err := handler.CreateProjectResources(projectName, []*keptnmodels.Resource{&resource})
	if err != nil {
		return nil, fmt.Errorf("Storing %s file failed. %s", resource.ResourceURI, err.Error())
	}

	logger.Info(fmt.Sprintf("Resource %s successfully stored", resource.ResourceURI))
	return &keptnmodels.Version{Version: versionStr}, nil
}

// logErrAndRespondWithDoneEvent sends a keptn done event to the keptn eventbroker
func logErrAndRespondWithDoneEvent(event cloudevents.Event, version *keptnmodels.Version, err error, logger keptnutils.Logger, ws *websocket.Conn) error {
	var result = "success"
	var webSocketMessage = "Shipyard successfully processed"
	var eventMessage = "Project created and shipyard successfully processed"

	if err != nil { // error
		result = "error"
		eventMessage = fmt.Sprintf("%s.", err.Error())
		webSocketMessage = eventMessage
		logger.Error(eventMessage)
	} else { // success
		logger.Info(eventMessage)
	}

	if err := keptnutils.WriteWSLog(ws, createEventCopy(event, "sh.keptn.events.log"), webSocketMessage, true, "INFO"); err != nil {
		logger.Error(fmt.Sprintf("Could not write log to websocket. %s", err.Error()))
	}
	if err := sendDoneEvent(event, result, eventMessage, version); err != nil {
		logger.Error(fmt.Sprintf("No sh.keptn.event.done event sent. %s", err.Error()))
	}

	return err
}

// createProject creates a project by using the configuration-service
func (client *Client) createProject(project keptnmodels.Project, logger keptnutils.Logger) error {
	configServiceURL, err := getServiceEndpoint(configservice)
	if err != nil {
		logger.Error(fmt.Sprintf("Could not get service endpoint for %s: %s", configservice, err.Error()))
		return err
	}
	prjHandler := keptnutils.NewProjectHandler(configServiceURL.String())
	errorObj, err := prjHandler.CreateProject(project)

	if errorObj == nil && err == nil {
		logger.Info("Project successfully created")
		return nil
	} else if errorObj != nil {
		return errors.New(*errorObj.Message)
	}
	return fmt.Errorf("Error in creating new project: %s", err.Error())
}

// createStage creates a stage by using the configuration-service
func (client *Client) createStage(project string, stage string, logger keptnutils.Logger) error {

	configServiceURL, err := getServiceEndpoint(configservice)
	if err != nil {
		logger.Error(fmt.Sprintf("Could not get service endpoint for %s: %s", configservice, err.Error()))
		return err
	}
	handler := keptnutils.NewStageHandler(configServiceURL.String())
	errorObj, err := handler.CreateStage(project, stage)

	if errorObj == nil && err == nil {
		logger.Info("Stage successfully created")
		return nil
	} else if errorObj != nil {
		return errors.New(*errorObj.Message)
	}
	return fmt.Errorf("Error in creating new stage: %s", err.Error())
}

// getServiceEndpoint retrieves an endpoint stored in an environment variable and sets http as default scheme
func getServiceEndpoint(service string) (url.URL, error) {
	url, err := url.Parse(os.Getenv(service))
	if err != nil {
		return *url, fmt.Errorf("Failed to retrieve value from ENVIRONMENT_VARIABLE: %s", service)
	}

	if url.Scheme == "" {
		url.Scheme = "http"
	}

	return *url, nil
}

// createEventCopy creates a deep copy of a CloudEvent
func createEventCopy(eventSource cloudevents.Event, eventType string) cloudevents.Event {
	var shkeptncontext string
	eventSource.Context.ExtensionAs("shkeptncontext", &shkeptncontext)
	var shkeptnphaseid string
	eventSource.Context.ExtensionAs("shkeptnphaseid", &shkeptnphaseid)
	var shkeptnphase string
	eventSource.Context.ExtensionAs("shkeptnphase", &shkeptnphase)
	var shkeptnstepid string
	eventSource.Context.ExtensionAs("shkeptnstepid", &shkeptnstepid)
	var shkeptnstep string
	eventSource.Context.ExtensionAs("shkeptnstep", &shkeptnstep)

	source, _ := url.Parse("shipyard-service")
	contentType := "application/json"

	event := cloudevents.Event{
		Context: cloudevents.EventContextV02{
			ID:          uuid.New().String(),
			Type:        eventType,
			Source:      types.URLRef{URL: *source},
			ContentType: &contentType,
			Extensions: map[string]interface{}{
				"shkeptncontext": shkeptncontext,
				"shkeptnphaseid": shkeptnphaseid,
				"shkeptnphase":   shkeptnphase,
				"shkeptnstepid":  shkeptnstepid,
				"shkeptnstep":    shkeptnstep,
			},
		}.AsV02(),
	}

	return event
}

// sendDoneEvent prepares a keptn done event and sends it to the eventbroker
func sendDoneEvent(receivedEvent cloudevents.Event, result string, message string, version *keptnmodels.Version) error {

	doneEvent := createEventCopy(receivedEvent, "sh.keptn.events.done")

	eventData := doneEventData{
		Result:  result,
		Message: message,
	}

	if version != nil {
		eventData.Version = version.Version
	}

	doneEvent.Data = eventData

	endPoint, err := getServiceEndpoint(eventbroker)
	if err != nil {
		return errors.New("Failed to retrieve endpoint of eventbroker. %s" + err.Error())
	}

	if endPoint.Host == "" {
		return errors.New("Host of eventbroker not set")
	}

	transport, err := cloudeventshttp.New(
		cloudeventshttp.WithTarget(endPoint.String()),
		cloudeventshttp.WithEncoding(cloudeventshttp.StructuredV02),
	)
	if err != nil {
		return errors.New("Failed to create transport: " + err.Error())
	}

	client, err := client.New(transport)
	if err != nil {
		return errors.New("Failed to create HTTP client: " + err.Error())
	}

	if _, err := client.Send(context.Background(), doneEvent); err != nil {
		return errors.New("Failed to send cloudevent sh.keptn.events.done: " + err.Error())
	}

	return nil
}