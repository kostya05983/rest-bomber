package core

import (
	"encoding/json"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/bomber-team/bomber-proto-contracts/golang/rest_contracts"
	"github.com/bomber-team/bomber-proto-contracts/golang/system"
	"github.com/bomber-team/rest-bomber/generators"
	"github.com/bomber-team/rest-bomber/nats_listener"
	"github.com/nats-io/nats.go"
	"github.com/sirupsen/logrus"
	"github.com/valyala/fasthttp"
)

type Core struct {
	publisher              *nats_listener.Publisher
	config                 *nats_listener.NatsConnectionConfiguration
	currentStatusBomber    system.StatusBomber
	dataAttack             []fasthttp.Request
	httpClient             *http.Transport
	resultsAttack          map[int32]int64 // amount statuses per status
	resultTimeouts         int64           // amount time out requests
	resultTimesForRequests []int64         // amount ms for one request
	attackReady            bool            // ready for attack?
	bomberIp               string
	formId                 string
}

var saveResults sync.Mutex

type SliceResult struct {
	Status      int
	TimeElapsed int64
	Timeout     bool
}

func (core *Core) CheckReady() bool {
	return core.attackReady
}

const (
	topicName    = "bomber.results"
	bomberResult = "bomber.result"
)

const (
	currentWorkers = 100
)

const (
	MaxIdleConnections int = 20
	RequestTimeout     int = 5
)

// createHTTPClient for connection re-use
func createHTTPClient() *http.Client {
	client := &http.Client{
		Transport: &http.Transport{
			MaxIdleConnsPerHost: MaxIdleConnections,
		},
		Timeout: time.Second * 30,
	}

	return client
}

func NewCore(conn *nats.Conn, bomberIp string) *Core {
	return &Core{
		publisher:              nats_listener.NewPublisher(conn),
		currentStatusBomber:    system.StatusBomber_UP,
		httpClient:             &http.Transport{},
		bomberIp:               bomberIp,
		resultTimesForRequests: []int64{},
	}
}

type RequestPayload struct {
	Request *fasthttp.Request
	Id      int
}

func (core *Core) preparingBody(bodyParams []*rest_contracts.BodyParam) ([]byte, error) {
	var resultBody map[string]interface{} = nil
	for _, value := range bodyParams {
		if value.IsGenerated {
			switch x := value.Config.Res.(type) {
			case *rest_contracts.GeneratorConfig_WordGeneratorConfig:
				resultBody[value.Name] = generators.GenerateWord(*x)
			case *rest_contracts.GeneratorConfig_DigitGeneratorConfig:
				resultBody[value.Name] = generators.GenerateDigits(*x)
			case *rest_contracts.GeneratorConfig_RegexpConfig:
				resultBody[value.Name] = generators.GenerateByRegexp(x)
			default:
				continue
			}
		} else {
			resultBody[value.Name] = value
		}
	}
	resultMarshaled, err := json.Marshal(resultBody)
	if err != nil {
		logrus.Error("error whilte marshaled body..")
		return nil, err
	}
	return resultMarshaled, nil
}

func (core *Core) prepareRequestParams(requestParams []*rest_contracts.RequestParam) string {
	if len(requestParams) == 0 {
		return ""
	}
	var resultUrlQueries string = "?"
	for index, value := range requestParams {
		if value.IsGeneratorNeed {
			switch x := value.GeneratorConfig.Res.(type) {
			case *rest_contracts.GeneratorConfig_WordGeneratorConfig:
				resultUrlQueries += value.Name + "=" + generators.GenerateWord(*x)
			case *rest_contracts.GeneratorConfig_DigitGeneratorConfig:
				generatedValue := generators.GenerateDigits(*x)

				resultUrlQueries += value.Name + "=" + strconv.Itoa(int(generatedValue))
			case *rest_contracts.GeneratorConfig_RegexpConfig:
				resultUrlQueries += value.Name + "=" + generators.GenerateByRegexp(x)
			default:
				continue
			}
		} else {
			resultUrlQueries += value.Name + "=" + value.Value
		}
		if index != len(requestParams)-1 {
			resultUrlQueries += "&"
		}
	}
	return resultUrlQueries
}

func (core *Core) enhancedHeadersInRequest(request *fasthttp.Request, task rest_contracts.Task) *fasthttp.Request {
	for key, value := range task.Schema.Headers {
		request.Header.Set(key, value)
	}
	return request
}

func (core *Core) preparingRequest(restTask *rest_contracts.Task) (*fasthttp.Request, error) {
	body, err := core.preparingBody(restTask.Schema.Body)
	if err != nil {
		return nil, err
	}
	urlParams := core.prepareRequestParams(restTask.Schema.Request)
	req := fasthttp.AcquireRequest()
	req.SetBody(body)
	req.SetRequestURI(restTask.Script.Address + urlParams)
	return core.enhancedHeadersInRequest(req, *restTask), nil
}

func (core *Core) cleanCurrentResults() {
	core.dataAttack = []fasthttp.Request{}
	core.resultTimeouts = 0
	core.resultTimesForRequests = []int64{}
	core.resultsAttack = map[int32]int64{}
	core.attackReady = false
}

func (core *Core) PreparingData(task rest_contracts.Task) {
	core.cleanCurrentResults()
	var index int64 = 0
	amountRequests := task.Script.Config.Rps * task.Script.Config.Time
	resultSliceRequests := make([]fasthttp.Request, amountRequests)
	for ; index < amountRequests; index++ {
		newRequest, errFormRequest := core.preparingRequest(&task)
		if errFormRequest != nil {
			logrus.Error("Can not forming request: ", errFormRequest)
			continue
		}
		resultSliceRequests[index] = *newRequest
	}
	core.dataAttack = resultSliceRequests
	core.formId = task.FormId
	core.attackReady = true
}

func (core *Core) resultHandler(resultChan chan SliceResult, completed chan bool, wg *sync.WaitGroup) {
	var countRequests int = 0
	logrus.Info("All requests: ", len(core.dataAttack))
	for {
		newRes := <-resultChan
		logrus.Info("Start preparing result: ", newRes)
		countRequests++
		saveResults.Lock()
		if newRes.Timeout {
			core.resultTimeouts++
		} else {
			core.resultsAttack[int32(newRes.Status)]++
			core.resultTimesForRequests = append(core.resultTimesForRequests, newRes.TimeElapsed)
		}
		saveResults.Unlock()
		if countRequests == len(core.dataAttack)-1 {
			completed <- true
			wg.Done()
			return
		}
	}
}

func (core *Core) runWorkers(task chan RequestPayload, completed chan bool, resultChan chan SliceResult) {
	for {
		select {
		case newRequest := <-task:
			resp := fasthttp.AcquireResponse()
			timeStart := time.Now()
			if err := fasthttp.Do(newRequest.Request, resp); err != nil {
				logrus.Error("Error while request: ", err)
				resultChan <- SliceResult{
					Timeout: true,
				}
				continue
			}
			durationTime := time.Since(timeStart)
			resultChan <- SliceResult{
				Status:      resp.StatusCode(),
				TimeElapsed: durationTime.Nanoseconds(),
			}
		case <-completed:
			logrus.Info("Completed requests")
			return
		}
	}
}

func (core *Core) startAttack(taskRunner chan RequestPayload) error {
	core.currentStatusBomber = system.StatusBomber_WORKING
	for index, request := range core.dataAttack {
		taskRunner <- RequestPayload{
			Request: &request,
			Id:      index,
		}
	}
	return nil
}

func (core *Core) FormResultAttack() *rest_contracts.BomberResult {
	return &rest_contracts.BomberResult{
		BomberIp:                core.bomberIp,
		FormId:                  core.formId,
		AmountTimeoutsRequests:  core.resultTimeouts,
		AmountStatusesPerStatus: core.resultsAttack,
		MsPerRequest:            core.resultTimesForRequests,
	}
}

func (core *Core) Start(task rest_contracts.Task, wg *sync.WaitGroup) {
	taskRunner := make(chan RequestPayload, currentWorkers)
	completed := make(chan bool)
	taskResult := make(chan SliceResult, currentWorkers)
	var index int64 = 0
	for ; index < task.Script.Config.Rps*task.Script.Config.Time; index++ {
		go core.runWorkers(taskRunner, completed, taskResult)
	}
	go core.resultHandler(taskResult, completed, wg)
	core.startAttack(taskRunner)
	logrus.Info("Attack was started")
	<-completed
	logrus.Info("Attack was completed")
}

func (core *Core) InitializeService() {
	core.changeStatusBomber(core.currentStatusBomber)
}

func (core *Core) handlingChangeStatusBomber() {
	currentStatus := core.currentStatusBomber
	for {
		time.Sleep(time.Second * 5)
		if currentStatus != core.currentStatusBomber {
			logrus.Info("Handled changing current status worker: ", core.currentStatusBomber.String())
			core.changeStatusBomber(core.currentStatusBomber)
			currentStatus = core.currentStatusBomber
		}
	}
}

func (core *Core) gracefullDownService() {
	logrus.Info("Graceful down service")
	core.changeStatusBomber(system.StatusBomber_DOWN)
}

func (core *Core) changeStatusBomber(status system.StatusBomber) {
	statusBomberInitialized := system.BomberStatusChange{
		BomberId:     core.config.CurrentServiceID,
		StatusBomber: status,
	}
	data, errMarshaling := statusBomberInitialized.Marshal()
	if errMarshaling != nil {
		logrus.Error("Can not marshaled payload for bomber server: ", errMarshaling)
	}
	if errPublish := core.publisher.PublishNewMessage(topicName, data); errPublish != nil {
		logrus.Error("Can not publish message into broker nats")
	}
}
