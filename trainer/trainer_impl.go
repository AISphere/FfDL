/*
 * Copyright 2017-2018 IBM Corporation
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 * http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package trainer

import (
	"archive/zip"
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/AISphere/ffdl-commons/config"
	"github.com/AISphere/ffdl-commons/logger"
	"github.com/AISphere/ffdl-commons/metricsmon"
	"github.com/AISphere/ffdl-lcm/service"
	tdsClient "github.com/AISphere/ffdl-model-metrics/client"
	tdsService "github.com/AISphere/ffdl-model-metrics/service/grpc_training_data_v1"
	trainerClient "github.com/AISphere/ffdl-trainer/client"
	"github.com/AISphere/ffdl-trainer/instrumentation"
	client "github.com/AISphere/ffdl-trainer/lcm-client"
	rlClient "github.com/AISphere/ffdl-trainer/plugins/ratelimiter"
	rlService "github.com/AISphere/ffdl-trainer/plugins/ratelimiter/service/grpc_ratelimiter_v1"
	"github.com/AISphere/ffdl-trainer/storage"
	"github.com/AISphere/ffdl-trainer/trainer/grpc_trainer_v2"
	"github.com/go-kit/kit/metrics"
	"github.com/go-kit/kit/metrics/discard"
	"github.com/nu7hatch/gouuid"
	"github.com/sirupsen/logrus"
	"github.com/spf13/viper"
	"github.com/ventu-io/go-shortid"
	"golang.org/x/net/context"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"gopkg.in/mgo.v2"
	"gopkg.in/yaml.v2"
	v1resource "k8s.io/apimachinery/pkg/api/resource"
)

const internalObjectStoreID = "dlaas_internal_os"

const (
	modelsBucketKey        = "objectstore.bucket.models"
	trainedModelsBucketKey = "objectstore.bucket.trainedmodels"

	defaultTrainedModelsBucket = "dlaas-trained-models"

	collectionNameTrainingJobs = "training_jobs"
	collectionNameJobHistory   = "job_history"

	debugLogsMode = false

	oldEndpointInternalPageSize = 10

	mongoAddressKey  = "mongo.address"
	mongoDatabaseKey = "mongo.database"
	mongoUsernameKey = "mongo.username"
	mongoPasswordKey = "mongo.password"

	gpuLimitsKey          = "gpu.limits"
	gpuLimitsQuerySizeKey = "gpu.limits.query.size"
	queueSizeLimitKey     = "queue.size.limit"
	noResultBucketTag     = "none"
	pollIntervalKey       = "queue.poll.interval"
)

const (
	// Used by a counter metric.
	dlaasStoreKind = "dlaas"
	userStoreKind  = "user"
)

// Confuse `go vet' to not check this `Errorf' call. :(
// See https://github.com/grpc/grpc-go/issues/90
var gerrf = status.Errorf

/*
	METRICS FOR TRAINER
*/
type trainerMetrics struct {
	createTrainingMetricsBunch        createTrainingMetricsBunchStruct
	createTrainingJobCounter          metrics.Counter
	createTrainingJobCounterNoLabel   metrics.Counter
	createTrainingJobGauge            metrics.Gauge
	deleteTrainingJobCounter          metrics.Counter
	haltTrainingJobCounter            metrics.Counter
	downloadTrainedModelJobCounter    metrics.Counter
	downloadTrainingMetricsJobCounter metrics.Counter
	rateLimitTrainingJobCounter       metrics.Counter
	trainingJobFailedCounter          metrics.Counter
	trainingJobFailedMetricsBunch     failedTrainingMetricsBunchStruct
	trainingJobFailedGauge            metrics.Gauge
	trainingJobSucceededCounter       metrics.Counter
	uploadModelFailedCounter          metrics.Counter
	enqueueJobCounter                 metrics.Counter
	dequeueJobCounter                 metrics.Counter
	deleteJobFromQueueCounter         metrics.Counter
	queueSizeGauge                    metrics.Gauge
	clusterWideGPUUsageGauge          metrics.Gauge
	clusterWideGPUUsageCounter        metrics.Counter
	createTrainingDuration            metrics.Histogram
	trainerServiceRestartCounter      metrics.Counter
	trainerUsageCounter               metrics.Counter
	trainerUsageGauge                 metrics.Gauge
}

//Metrics for tracking trainings created
type createTrainingMetricsBunchStruct struct {
	createTrainingJobFrameworkCounter metrics.Counter
	createTrainingJobGPUTypeCounter   metrics.Counter
	createTrainingJobCPUCounter       metrics.Counter
	createTrainingJobGPUCounter       metrics.Counter
}

func (metrics *createTrainingMetricsBunchStruct) incrementCreateTrainingMetrics(framework string, version string, gpuType string, cpus string, gpus string) {
	metrics.createTrainingJobFrameworkCounter.With("framework", framework, "version", version).Add(1)
	metrics.createTrainingJobGPUTypeCounter.With("gpuType", gpuType).Add(1)
	metrics.createTrainingJobCPUCounter.With("cpus", cpus).Add(1)
	metrics.createTrainingJobGPUCounter.With("gpus", gpus).Add(1)
}

//Metrics for tracking failed trainings
type failedTrainingMetricsBunchStruct struct {
	trainingJobFailedFrameworkCounter metrics.Counter
	trainingJobFailedGPUTypeCounter   metrics.Counter
	trainingJobFailedCPUCounter       metrics.Counter
	trainingJobFailedGPUCounter       metrics.Counter
	clientServerErrorMetricsBunch     clientServerErrorMetricsBunchStruct
}

func (metrics *failedTrainingMetricsBunchStruct) incrementFailedTrainingMetrics(framework string, version string, gpuType string, cpus string, gpus string, errorcode string) {
	errtype := metrics.clientServerErrorMetricsBunch.incrementClientServerErrorMetrics(errorcode)
	metrics.trainingJobFailedFrameworkCounter.With("type", errtype, "framework", framework, "version", version).Add(1)
	metrics.trainingJobFailedGPUTypeCounter.With("type", errtype, "gpuType", gpuType).Add(1)
	metrics.trainingJobFailedCPUCounter.With("type", errtype, "cpus", cpus).Add(1)
	metrics.trainingJobFailedGPUCounter.With("type", errtype, "gpus", gpus).Add(1)
}

//Metrics for client and server error for trainer
type clientServerErrorMetricsBunchStruct struct {
	trainingJobFailedServerCounter metrics.Counter
	trainingJobFailedClientCounter metrics.Counter
}

func (metrics *clientServerErrorMetricsBunchStruct) incrementClientServerErrorMetrics(errorcode string) string {
	errType := "server"
	if strings.HasPrefix(errorcode, "C") {
		metrics.trainingJobFailedClientCounter.With("errorcode", errorcode).Add(1)
		errType = "client"
	} else {
		metrics.trainingJobFailedServerCounter.With("errorcode", errorcode).Add(1)
	}
	return errType
}

// Service represents the functionality of the trainer service
type Service interface {
	grpc_trainer_v2.TrainerServer
	service.LifecycleHandler
	StopTrainer()
}

type queueHandler struct {
	stopQueue chan struct{}
	*TrainingJobQueue
}

type trainerService struct {
	mtx                 sync.RWMutex //this lock should only be used for instantiating job queue
	datastore           storage.DataStore
	lcm                 client.LcmClient
	repo                repository
	jobHistoryRepo      jobHistoryRepository
	modelsBucket        string
	trainedModelsBucket string
	metrics             *trainerMetrics
	resettableMetrics   *ResettableMetrics
	tds                 tdsClient.TrainingDataClient
	queues              map[string]*queueHandler
	queuesStarted       bool
	ratelimiter         rlClient.RatelimiterClient
	gpuAvailable        map[string]int64
	service.Lifecycle
}

// NewService creates a new trainer service.
func NewService() Service {
	logr := logger.LogServiceBasic(logger.LogkeyTrainerService)

	logr.Info("Entry into NewService()")

	config.FatalOnAbsentKey(mongoAddressKey)
	config.SetDefault(gpuLimitsQuerySizeKey, 200)
	config.SetDefault(pollIntervalKey, 60) // in seconds
	config.SetDefault(queueSizeLimitKey, 0)

	resettableMetrics := NewResettableMetrics() //init resettable metrics, reset after 24 hours

	trainerMetrics := trainerMetrics{
		createTrainingJobCounter:        metricsmon.NewCounter("trainer_trainings_create_total", "Metrics for total number of training jobs created", []string{"framework", "version", "gpus", "cpus", "gpuType", "memory"}),
		createTrainingJobCounterNoLabel: metricsmon.NewCounter("trainer_trainings_create_total_no_label", "Metrics for total number of training jobs created (No labels)", []string{}),
		createTrainingMetricsBunch: createTrainingMetricsBunchStruct{
			createTrainingJobFrameworkCounter: metricsmon.NewCounter("trainer_trainings_framework_create_total", "Metrics for total number of training jobs created per framework", []string{"framework", "version"}),
			createTrainingJobGPUTypeCounter:   metricsmon.NewCounter("trainer_trainings_gpuType_create_total", "Metrics for total number of training jobs created per gpuType", []string{"gpuType"}),
			createTrainingJobCPUCounter:       metricsmon.NewCounter("trainer_trainings_cpus_create_total", "Metrics for total number of training jobs created per cpus", []string{"cpus"}),
			createTrainingJobGPUCounter:       metricsmon.NewCounter("trainer_trainings_gpus_create_total", "Metrics for total number of training jobs created per gpus", []string{"gpus"}),
		},
		createTrainingJobGauge:            resettableMetrics.NewGauge("trainer_trainings_create_total_gauge_reset", "Metrics for total number of training jobs created overall (reset per day)"),
		deleteTrainingJobCounter:          metricsmon.NewCounter("trainer_trainings_delete_total", "Metrics for total number of training jobs deleted", []string{}),
		haltTrainingJobCounter:            metricsmon.NewCounter("trainer_trainings_halt_total", "Metrics for total number of training jobs halted", []string{}),
		downloadTrainedModelJobCounter:    metricsmon.NewCounter("trainer_model_download_total", "Metrics for total number of trained models downloaded", []string{}),
		downloadTrainingMetricsJobCounter: metricsmon.NewCounter("trainer_metrics_download_total", "Metrics for total number of training metrics downloaded", []string{}),
		rateLimitTrainingJobCounter:       metricsmon.NewCounter("trainer_ratelimitinvocations_total", "Metrics for total rate limit invocations on trainer", []string{}),
		trainingJobFailedCounter:          metricsmon.NewCounter("trainer_trainings_failed_total", "Metrics for failed training jobs", []string{"framework", "version", "gpus", "cpus", "memory", "type", "errorcode"}),
		trainingJobFailedMetricsBunch: failedTrainingMetricsBunchStruct{
			trainingJobFailedFrameworkCounter: metricsmon.NewCounter("trainer_trainings_framework_failed_total", "Metrics for failed training jobs per framework", []string{"type", "framework", "version"}),
			trainingJobFailedGPUTypeCounter:   metricsmon.NewCounter("trainer_trainings_gpuType_failed_total", "Metrics for failed training jobs per gpuType", []string{"type", "gpuType"}),
			trainingJobFailedCPUCounter:       metricsmon.NewCounter("trainer_trainings_cpus_failed_total", "Metrics for failed training jobs with per cpus", []string{"type", "cpus"}),
			trainingJobFailedGPUCounter:       metricsmon.NewCounter("trainer_trainings_gpus_failed_total", "Metrics for failed training jobs with per gpus", []string{"type", "gpus"}),
			clientServerErrorMetricsBunch: clientServerErrorMetricsBunchStruct{
				trainingJobFailedServerCounter: metricsmon.NewCounter("trainer_trainings_server_failed_total", "Metrics for failed training jobs with Server errors", []string{"errorcode"}),
				trainingJobFailedClientCounter: metricsmon.NewCounter("trainer_trainings_client_failed_total", "Metrics for failed training jobs with Client errors", []string{"errorcode"}),
			},
		},
		trainingJobFailedGauge:       resettableMetrics.NewGauge("trainer_trainings_failed_total_gauge_reset", "Metrics for failed training jobs overall (reset per day)"),
		trainingJobSucceededCounter:  metricsmon.NewCounter("trainer_trainings_success_total", "Metrics for succeeded training jobs", []string{"framework", "version", "gpus", "cpus", "memory"}),
		clusterWideGPUUsageGauge:     metricsmon.NewGauge("trainer_cluster_wide_gpu_usage", "metrics for cluster wide gpu usage", []string{"gpuType"}),
		clusterWideGPUUsageCounter:   metricsmon.NewCounter("trainer_cluster_wide_gpu_usage_total", "metrics for cluster wide gpu usage counter", []string{"gpuType", "gpus"}),
		trainerServiceRestartCounter: metricsmon.NewCounter("trainer_service_restart_total", "Metrics for trainer service restarts because of failures", []string{"reason"}),
		trainerUsageCounter:          metricsmon.NewCounter("trainer_usage_count", "Metrics for trainer for user usages counter", []string{"userid", "gpuType", "framework", "version"}),
		trainerUsageGauge:            metricsmon.NewGauge("trainer_usage_gauge", "Metrics for trainer for user usages gauge", []string{"userid", "gpuType", "framework", "version"}),

		// The "kind" is either "dlaas" for dlaas object store, or "user" for the user's object store
		uploadModelFailedCounter: metricsmon.NewCounter("trainer_uploadmodel_failed_total", "Metrics for failed uploads of model definition", []string{"kind"}),

		enqueueJobCounter:         metricsmon.NewCounter("trainer_jobs_enqueued_total", "Metrics for number of jobs enqueued", []string{}),
		dequeueJobCounter:         metricsmon.NewCounter("trainer_jobs_dequeued_total", "Metrics for number of jobs dequeued", []string{}),
		deleteJobFromQueueCounter: metricsmon.NewCounter("trainer_jobs_queue_deleted_total", "Metrics for number of jobs deleted from queue", []string{}),
		queueSizeGauge:            metricsmon.NewGauge("trainer_queue_size", "Metrics for queue size", []string{"gpuType"}),

		createTrainingDuration: metricsmon.NewSummary("trainer_create_time_duration", "Time duration for create training job", []string{}),
	}

	logr.Info("Calling initMetrics()")
	initMetrics(&trainerMetrics)
	var ds storage.DataStore
	var err error
	dsType := config.GetDataStoreType()
	if dsType != "" {
		ds, err = storage.CreateDataStore(dsType, config.GetDataStoreConfig())
		if err != nil {
			logr.WithError(err).Fatalf("Cannot create datastore")
			trainerMetrics.trainerServiceRestartCounter.With("reason", "datastore").Add(1)
		}
		err = ds.Connect()
		if err != nil {
			logr.WithError(err).Fatalf("Cannot connect to object store")
			trainerMetrics.trainerServiceRestartCounter.With("reason", "objectstore").Add(1)
		}
		logr.Infof("Using dlaas object store of type %s", dsType)
	} else {
		logr.Infof("Not using a dlaas object store")
	}

	logr.Info("Calling newTrainingsRepository()")
	repo, err := newTrainingsRepository(viper.GetString(mongoAddressKey),
		viper.GetString(mongoDatabaseKey), viper.GetString(mongoUsernameKey),
		viper.GetString(mongoPasswordKey), config.GetMongoCertLocation(), "training_jobs")
	if err != nil {
		logr.WithError(err).Fatalf("Cannot create repository with %s %s %s", viper.GetString(mongoAddressKey), viper.GetString(mongoDatabaseKey), viper.GetString(mongoUsernameKey))
		trainerMetrics.trainerServiceRestartCounter.With("reason", "createrepository").Add(1)
	}
	logr.Info("back from newTrainingsRepository()")
	jobHistoryRepo, err := newJobHistoryRepository(viper.GetString(mongoAddressKey),
		viper.GetString(mongoDatabaseKey), viper.GetString(mongoUsernameKey),
		viper.GetString(mongoPasswordKey), config.GetMongoCertLocation(), collectionNameJobHistory)
	if err != nil {
		logr.WithError(err).Fatalf("Cannot create repository with %s %s %s %s", viper.GetString("mongo.address"),
			viper.GetString(mongoDatabaseKey), viper.GetString(mongoUsernameKey), collectionNameJobHistory)
		trainerMetrics.trainerServiceRestartCounter.With("reason", "createrepository").Add(1)
	}

	queues := make(map[string]*queueHandler)
	gpuTypes := getAllResourceTypes()

	for _, gpuType := range gpuTypes {
		// only create a queue if there is a limit set
		if getGpuLimitByType(gpuType) > 0 {
			queue, err := newTrainingJobQueue(viper.GetString(mongoAddressKey),
				viper.GetString(mongoDatabaseKey), viper.GetString(mongoUsernameKey),
				viper.GetString(mongoPasswordKey), config.GetMongoCertLocation(), QueueName(gpuType), LockName(gpuType))
			if err != nil {
				logr.WithError(err).Fatalf("Cannot create queue with %s %s %s", viper.GetString(mongoAddressKey), viper.GetString(mongoDatabaseKey), viper.GetString(mongoUsernameKey))
				trainerMetrics.trainerServiceRestartCounter.With("reason", "createqueue").Add(1)
			}

			queues[TransformResourceName(gpuType)] = &queueHandler{make(chan struct{}), queue}
		}
	}

	anyQueue, err := newTrainingJobQueue(viper.GetString(mongoAddressKey),
		viper.GetString(mongoDatabaseKey), viper.GetString(mongoUsernameKey),
		viper.GetString(mongoPasswordKey), config.GetMongoCertLocation(), QueueName("ANY"), LockName("ANY"))
	if err != nil {
		logr.WithError(err).Fatalf("Cannot create queue with %s %s %s", viper.GetString(mongoAddressKey), viper.GetString(mongoDatabaseKey), viper.GetString(mongoUsernameKey))
		trainerMetrics.trainerServiceRestartCounter.With("reason", "createqueue").Add(1)
	}
	queues["ANY"] = &queueHandler{make(chan struct{}), anyQueue}

	gpuAvailable, err := getGpuMapFromConfig()
	if err != nil {
		logr.WithError(err).Fatalf("Could not parse gpu and limits from config file")
		trainerMetrics.trainerServiceRestartCounter.With("reason", "creategpumap").Add(1)
	}

	s := &trainerService{
		datastore:           ds,
		repo:                repo,
		jobHistoryRepo:      jobHistoryRepo,
		modelsBucket:        getModelsBucket(),
		trainedModelsBucket: getTrainedModelsBucket(),
		metrics:             &trainerMetrics,
		resettableMetrics:   resettableMetrics,
		queues:              queues,
		queuesStarted:       false,
		gpuAvailable:        gpuAvailable,
	}
	logr.Infof("Bucket for model definitions: %s", s.modelsBucket)
	logr.Infof("Bucket for trained models: %s", s.trainedModelsBucket)
	logr.Infof("Datastore type is of type: %s", fmt.Sprintf("%T", ds))

	logr.Info("calling RegisterTrainerServer()")
	s.RegisterService = func() {
		grpc_trainer_v2.RegisterTrainerServer(s.Server, s)
	}
	s.StartQueues()
	return s
}

// NewTestService creates a new service instance for testing
func NewTestService(ds storage.DataStore, repo repository, jobHistoryRepo jobHistoryRepository,
	lcm client.LcmClient, tds tdsClient.TrainingDataClient, ratelimiter rlClient.RatelimiterClient, queues map[string]*queueHandler) Service {

	logr := logger.LogServiceBasic(logger.LogkeyTrainerService)

	config.SetDefault(gpuLimitsQuerySizeKey, 100)
	config.SetDefault(pollIntervalKey, 1) // set poll interval lower to run tests faster

	resettableMetrics := NewResettableMetrics() //init resettable metrics, reset after 24 hours

	trainerMetrics := trainerMetrics{
		createTrainingJobCounter:        discard.NewCounter(),
		createTrainingJobCounterNoLabel: discard.NewCounter(),
		createTrainingMetricsBunch: createTrainingMetricsBunchStruct{
			createTrainingJobFrameworkCounter: discard.NewCounter(),
			createTrainingJobGPUTypeCounter:   discard.NewCounter(),
			createTrainingJobCPUCounter:       discard.NewCounter(),
			createTrainingJobGPUCounter:       discard.NewCounter(),
		},
		createTrainingJobGauge:            discard.NewGauge(),
		deleteTrainingJobCounter:          discard.NewCounter(),
		haltTrainingJobCounter:            discard.NewCounter(),
		downloadTrainedModelJobCounter:    discard.NewCounter(),
		downloadTrainingMetricsJobCounter: discard.NewCounter(),
		rateLimitTrainingJobCounter:       discard.NewCounter(),
		trainingJobFailedCounter:          discard.NewCounter(),
		trainingJobFailedMetricsBunch: failedTrainingMetricsBunchStruct{
			trainingJobFailedFrameworkCounter: discard.NewCounter(),
			trainingJobFailedGPUTypeCounter:   discard.NewCounter(),
			trainingJobFailedCPUCounter:       discard.NewCounter(),
			trainingJobFailedGPUCounter:       discard.NewCounter(),
			clientServerErrorMetricsBunch: clientServerErrorMetricsBunchStruct{
				trainingJobFailedServerCounter: discard.NewCounter(),
				trainingJobFailedClientCounter: discard.NewCounter(),
			},
		},
		trainingJobFailedGauge:       discard.NewGauge(),
		trainingJobSucceededCounter:  discard.NewCounter(),
		uploadModelFailedCounter:     discard.NewCounter(),
		enqueueJobCounter:            discard.NewCounter(),
		dequeueJobCounter:            discard.NewCounter(),
		queueSizeGauge:               discard.NewGauge(),
		deleteJobFromQueueCounter:    discard.NewCounter(),
		clusterWideGPUUsageGauge:     discard.NewGauge(),
		clusterWideGPUUsageCounter:   discard.NewCounter(),
		createTrainingDuration:       discard.NewHistogram(),
		trainerServiceRestartCounter: discard.NewCounter(),
		trainerUsageCounter:          discard.NewCounter(),
		trainerUsageGauge:            discard.NewGauge(),
	}

	gpuTypes := getAllResourceTypes()
	gpuAvailable := make(map[string]int64)
	for _, gpuType := range gpuTypes {
		gpuAvailable[TransformResourceName(gpuType)] = 1
	}

	s := &trainerService{
		datastore:           ds,
		repo:                repo,
		jobHistoryRepo:      jobHistoryRepo,
		lcm:                 lcm,
		modelsBucket:        getModelsBucket(),
		trainedModelsBucket: getTrainedModelsBucket(),
		metrics:             &trainerMetrics,
		resettableMetrics:   resettableMetrics,
		tds:                 tds,
		ratelimiter:         ratelimiter,
		queues:              queues,
		queuesStarted:       false,
		gpuAvailable:        gpuAvailable,
	}

	logr.Infof("Datastore type is of type: %s", fmt.Sprintf("%T", ds))
	if ds == nil {
		logr.Infof("Datastore value is nil")
	}

	s.RegisterService = func() {
		grpc_trainer_v2.RegisterTrainerServer(s.Server, s)
	}
	s.StartQueues()
	return s
}

func initMetrics(trainerMetrics *trainerMetrics) {
	metricClientErrorCodes := []string{"C101", "C102", "C103", "C104", "C201"}
	metricServerErrorCodes := []string{"S100", "S101", "S103", "S104", "S200", "S201", "S210", "S211", "S301", "S302", "S303", "S304", "S305"}
	metricGPUTypes := []string{"CPU", "nvidia-TeslaK80", "nvidia-TeslaP100", "nvidia-TeslaV100"}
	errTypes := []string{"server", "client"}

	//Client errors
	for _, errCode := range metricClientErrorCodes {
		trainerMetrics.trainingJobFailedMetricsBunch.clientServerErrorMetricsBunch.trainingJobFailedClientCounter.With("errorcode", errCode).Add(0)
	}
	//Server errors
	for _, errCode := range metricServerErrorCodes {
		trainerMetrics.trainingJobFailedMetricsBunch.clientServerErrorMetricsBunch.trainingJobFailedServerCounter.With("errorcode", errCode).Add(0)
	}
	//GPUType
	for _, GPUType := range metricGPUTypes {
		trainerMetrics.createTrainingMetricsBunch.createTrainingJobGPUTypeCounter.With("gpuType", GPUType).Add(0)
		for _, errType := range errTypes {
			trainerMetrics.trainingJobFailedMetricsBunch.trainingJobFailedGPUTypeCounter.With("type", errType, "gpuType", GPUType).Add(0)
		}
	}
	//CPU
	for i := 0; i < 100; i++ {
		trainerMetrics.createTrainingMetricsBunch.createTrainingJobCPUCounter.With("cpus", strconv.Itoa(i)).Add(0)
		for _, errType := range errTypes {
			trainerMetrics.trainingJobFailedMetricsBunch.trainingJobFailedCPUCounter.With("type", errType, "cpus", strconv.Itoa(i)).Add(0)
		}
	}
	//GPU
	for i := 0; i < 5; i++ {
		trainerMetrics.createTrainingMetricsBunch.createTrainingJobGPUCounter.With("gpus", strconv.Itoa(i)).Add(0)
		for _, errType := range errTypes {
			trainerMetrics.trainingJobFailedMetricsBunch.trainingJobFailedGPUCounter.With("type", errType, "gpus", strconv.Itoa(i)).Add(0)
		}
	}
}

func debugLogger(logrr *logrus.Entry, isEnabled bool) *logger.LocLoggingEntry {
	logr := new(logger.LocLoggingEntry)
	logr.Logger = logrr
	logr.Enabled = isEnabled

	return logr
}

// Cover for deprecated grpc function.
func grpcErrorDesc(err error) string {
	if s, ok := status.FromError(err); ok {
		return s.Message()
	}
	return err.Error()
}

func (s *trainerService) StartQueues() {
	logr := logger.LocLogger(logEntry())
	// ensure only one thread per trainer pulling jobs
	s.mtx.Lock()
	defer s.mtx.Unlock()
	if !s.queuesStarted {
		s.queuesStarted = true
		for gpuType, qHandler := range s.queues {
			logr.Debugf("starting queue for %s", gpuType)
			tick := time.NewTicker(time.Duration(viper.GetInt(pollIntervalKey)) * time.Second).C
			go func(gpuType string, qHandler *queueHandler) {
				for {
					select {
					case <-tick:
						s.pullJobFromQueue(gpuType)
					case <-qHandler.stopQueue:
						logr.Debugf("%s queue stopped", gpuType)
						return
					}
				}
			}(gpuType, qHandler)
		}
	}
}

func (s *trainerService) StopTrainer() {
	logr := logger.LocLogger(logEntry())
	logr.Debugf("stopping trainer")

	//stop resettable metrics ticker
	s.resettableMetrics.done <- struct{}{}

	// Close mongodb connections
	s.repo.Close()
	s.jobHistoryRepo.Close()
	for _, qHandler := range s.queues {
		qHandler.stopQueue <- struct{}{}
		close(qHandler.stopQueue)
		qHandler.TrainingJobQueue.session.Close()
	}
	s.Stop() // stop Service

}

func (s *trainerService) pullJobFromQueue(gpuType string) {
	logr := logger.LocLogger(logEntry())

	qHandler := s.queues[gpuType]
	if qHandler == nil {
		logr.Warnf("there is no queue for type %s", gpuType)
		return
	}

	locked := true
	qerr := qHandler.Lock()
	if qerr != nil {
		logr.WithError(qerr).Errorf("failed to lock %s queue", gpuType)
		return
	}
	defer func() {
		if locked {
			qHandler.Unlock()
		}
	}()

	qSize, err := qHandler.Size()
	logr.Infof("queue %s has %d elements", gpuType, qSize)
	s.metrics.queueSizeGauge.With("gpuType", gpuType).Set(float64(qSize))

	empty, err := qHandler.Empty()
	if err != nil {
		logr.WithError(err).Errorf("failed to check if %s queue is empty", gpuType)
		return
	}
	if empty {
		return
	}

	nextJobID, err := qHandler.Peek()
	if err != nil {
		logr.Errorf("failed to peek %s training job queue", gpuType)
		return
	}
	if nextJobID == "" {
		logr.Errorf("job pulled from %s queue is nil", gpuType)
		return
	}

	trainingRecord, err := s.repo.Find(nextJobID)
	if err != nil {
		if err == mgo.ErrNotFound {
			logr.Debugf("job %s not found in mongo, assuming job was deleted", nextJobID)
			qHandler.Delete(nextJobID)
			s.metrics.deleteJobFromQueueCounter.Add(1)
			return
		}
		logr.WithError(err).Errorf("error retrieving training job")
		return
	}

	if trainingRecord.Deleted {
		logr.Debugf("job %s was deleted", nextJobID)
		qHandler.Delete(nextJobID)
		s.metrics.deleteJobFromQueueCounter.Add(1)
		return
	}
	if trainingRecord.TrainingStatus.Status != grpc_trainer_v2.Status_QUEUED {
		logr.Warnf("job %s expected status QUEUED but found %s, removing job from queue", nextJobID, trainingRecord.TrainingStatus)
		qHandler.Delete(nextJobID)
		s.metrics.deleteJobFromQueueCounter.Add(1)
		return
	}

	logr.Debugf("got training job %s from %s queue", nextJobID, gpuType)

	rateLimited, zone, rlErr := s.rateLimitTrainingJob(trainingRecord, logr)
	if rateLimited {
		logr.Debugf("training job %s is rate-limited, leaving in %s queue", trainingRecord.TrainingID, gpuType)
		return
	}

	// set zone in training record, will be stored to mongo in submitJobToLCM
	trainingRecord.Zone = zone

	err = s.submitJobToLCM(trainingRecord, logr)
	if err != nil {
		// submitting to LCM failed, don't update job history or dequeue
		return
	}

	dequeuedJob, dequeueErr := qHandler.Dequeue()
	if dequeueErr != nil {
		logr.WithError(dequeueErr).Errorf("Failed to dequeue training job %s", trainingRecord.TrainingID)
	}
	if dequeueErr == nil && dequeuedJob != trainingRecord.TrainingID {
		logr.Errorf("expected to dequeue job %s, but got %s instead. This should never happen", trainingRecord.TrainingID, dequeuedJob)
		enqueueErr := qHandler.Enqueue(dequeuedJob)
		if enqueueErr != nil {
			logr.Errorf("job %s should not have been dequeued, and could not be re-enqueued. the record will stay in mongo but the job will never run", dequeuedJob)
			// find and update record with FAILED status
			if dequeuedTrainingRecord, err := s.repo.Find(dequeuedJob); err != nil {
				// this is only a problem if the dequeued job is still QUEUED, since it will stay QUEUED forever. if it has already been submitted to LCM, it should not be in the queue
				if dequeuedTrainingRecord.TrainingStatus.Status == grpc_trainer_v2.Status_QUEUED {
					_, err = updateTrainingJobPostLock(s, &grpc_trainer_v2.UpdateRequest{
						TrainingId:    dequeuedJob,
						UserId:        dequeuedTrainingRecord.UserID,
						Status:        grpc_trainer_v2.Status_FAILED,
						StatusMessage: "Job was dequeued without being submitted",
						ErrorCode:     trainerClient.ErrCodeFailDequeue,
					})
					if err != nil {
						logr.WithError(err).Errorln("Unable to update job status to FAILED")
					}
				}
			}
		}
	}
	s.metrics.dequeueJobCounter.Add(1)

	qHandler.Unlock()
	locked = false

	// store job state transition
	timestamp := trainerClient.CurrentTimestampAsString()
	e := &JobHistoryEntry{
		TrainingID:    trainingRecord.TrainingID,
		Timestamp:     timestamp,
		Status:        trainingRecord.TrainingStatus.Status,
		StatusMessage: trainingRecord.TrainingStatus.StatusMessage,
		ErrorCode:     trainingRecord.TrainingStatus.ErrorCode,
	}
	s.jobHistoryRepo.RecordJobStatus(e)

	// if the job is not ratelimited, keep trying to pull jobs until one is ratelimited
	if rlErr == nil {
		s.pullJobFromQueue(gpuType)
	}
}

func (s *trainerService) CreateTrainingJob(ctx context.Context, req *grpc_trainer_v2.CreateRequest) (*grpc_trainer_v2.CreateResponse, error) {
	duration := metrics.NewTimer(s.metrics.createTrainingDuration)
	defer duration.ObserveDuration()

	sid, _ := shortid.Generate()
	id := fmt.Sprintf("training-%s", sid)

	logr := logger.LocLogger(logWith(id, req.UserId))

	cl := instrumentation.NewCallLogger(ctx, "CreateTrainingJob", logr)
	defer cl.Returned()

	if err := s.validateRequest(logr.Logger, req); err != nil {
		return nil, err
	}

	if req.Training == nil || req.Training.Resources == nil {
		resourcesNotSpecifiedTypeError := fmt.Sprintf("user did not specify any valid training resources")
		logr.Errorf(resourcesNotSpecifiedTypeError)
		return nil, gerrf(codes.InvalidArgument, grpcErrorDesc(errors.New(resourcesNotSpecifiedTypeError)))
	}
	// Validate the gpu type
	gpuType := TransformResourceName(req.Training.Resources.GpuType)
	if gpuType != "" {
		gpuLimit, gpuFound := s.gpuAvailable[gpuType]
		if !gpuFound {
			invalidGpuTypeError := fmt.Sprintf("user entered an invalid gpu type: %s", gpuType)
			logr.Errorf(invalidGpuTypeError)
			return nil, gerrf(codes.InvalidArgument, grpcErrorDesc(errors.New(invalidGpuTypeError)))
		} else if gpuLimit == 0 {
			unsupportedGpuTypeError := fmt.Sprintf("user requested unsupported gpu type: %s", gpuType)
			logr.Errorf(unsupportedGpuTypeError)
			return nil, gerrf(codes.InvalidArgument, grpcErrorDesc(errors.New(unsupportedGpuTypeError)))
		}
	}
	setDefaultResourceRequirements(req.Training)

	cpuCount := v1resource.NewMilliQuantity(int64(float64(req.Training.Resources.Cpus)*1000.0), v1resource.DecimalSI)
	memCount := fmt.Sprintf("%v-%v", req.Training.Resources.Memory, req.Training.Resources.MemoryUnit)
	//request is validated, now bump up the counter
	logFrameworkVersionValue := fmt.Sprintf("%s-%s", req.ModelDefinition.Framework.Name, req.ModelDefinition.Framework.Version)
	logGpuTypeUsagesValue := fmt.Sprintf("%s-%v", req.Training.Resources.GpuType, req.Training.Resources.Gpus)
	logr = logr.WithFields(logrus.Fields{
		logger.LogkeyFramework:        req.ModelDefinition.Framework.Name,
		logger.LogkeyFrameworkVersion: logFrameworkVersionValue,
		logger.LogkeyGpuType:          req.Training.Resources.GpuType,
		logger.LogkeyGpuUsage:         logGpuTypeUsagesValue,
		"image_tag":                   req.ModelDefinition.Framework.ImageTag,
		"cpu_usage":                   cpuCount,
		"memory":                      memCount,
	})

	logr.Infof(" metrics for total number of training jobs ")

	s.metrics.createTrainingJobCounter.With("framework", req.ModelDefinition.Framework.Name,
		"version", req.ModelDefinition.Framework.Version,
		"gpus", strconv.Itoa(int(req.Training.Resources.Gpus)),
		"cpus", strconv.Itoa(int(req.Training.Resources.Cpus)),
		"gpuType", req.Training.Resources.GpuType,
		"memory", strconv.Itoa(int(req.Training.Resources.Memory))).Add(1)

	s.metrics.createTrainingMetricsBunch.incrementCreateTrainingMetrics(req.ModelDefinition.Framework.Name, req.ModelDefinition.Framework.Version, req.Training.Resources.GpuType,
		strconv.Itoa(int(req.Training.Resources.Cpus)), strconv.Itoa(int(req.Training.Resources.Gpus)))

	s.metrics.createTrainingJobCounterNoLabel.Add(1)

	//Resettable metrics
	s.metrics.createTrainingJobGauge.Add(1)

	outputDatastore := s.getOutputDatastore(req.Training.OutputData, req.Datastores)
	// upload model definition ZIP file to object store and set location
	if req.ModelDefinition.Content != nil && strings.ToLower(outputDatastore.Fields["bucket"]) != noResultBucketTag {
		// Upload to DLaaS Object store, if there's one defined.
		if s.datastore != nil {
			err := s.datastore.UploadArchive(s.modelsBucket, getModelZipFileName(id), req.ModelDefinition.Content)
			if err != nil {
				logr.WithError(err).Errorf("Error uploading model to object store")
				s.metrics.uploadModelFailedCounter.With("kind", dlaasStoreKind).Add(1)
				return nil, err
			}
			req.ModelDefinition.Location = fmt.Sprintf("%s/%s.zip", s.modelsBucket, id)
			cl.Observe("uploaded model to dlaas object store")
		} else {
			logr.Infof("Not uploading model to dlaas object store")
		}

		// Upload to user's result object store.
		ds, err := storage.CreateDataStore(outputDatastore.Type, outputDatastore.Connection)
		if err != nil {
			s.metrics.uploadModelFailedCounter.With("kind", userStoreKind).Add(1)
			logr.WithError(err).Fatalf("Cannot create datastore for output data store %s", outputDatastore.Id)
			return nil, err
		}
		err = ds.Connect()
		if err != nil {
			s.metrics.uploadModelFailedCounter.With("kind", userStoreKind).Add(1)
			logr.WithError(err).Fatalf("Cannot connect to output object store %s", outputDatastore.Id)
			return nil, err
		}
		defer ds.Disconnect()
		bucket := outputDatastore.Fields["bucket"]
		object := fmt.Sprintf("%s/_submitted_code/model.zip", id)
		logr.Infof("Writing to output object store: %s/%s", bucket, object)
		err = ds.UploadArchive(bucket, object, req.ModelDefinition.Content)
		if err != nil {
			s.metrics.uploadModelFailedCounter.With("kind", userStoreKind).Add(1)
			logr.WithError(err).Errorf("Error uploading model to output object store")
			return nil, err
		}
		req.ModelDefinition.Location = fmt.Sprintf("%s/%s", bucket, object) // may overwrite the path to the dlaas object store
		cl.Observe("uploaded model to user's object store")
	}

	// create a copy of the model definition without the content field (do not store it to the database)
	modelWithoutContent := *req.ModelDefinition
	modelWithoutContent.Content = nil

	// get evaluation metrics from create request
	evaluationMetricsSpec := ""
	if req.EvaluationMetrics != nil {
		logr.Debugf("EMExtractionSpec ImageTag: %s", req.EvaluationMetrics.ImageTag)
		wrapper := make(map[string]interface{})
		wrapper["evaluation_metrics"] = req.EvaluationMetrics
		data, err := yaml.Marshal(wrapper)
		if err != nil {
			logr.WithError(err).Errorf("Can't re-marshal evaluation metrics specification")
		}
		evaluationMetricsSpec = string(data)
		logr.Debugf("Set evaluation_metrics to: %s<eof>", evaluationMetricsSpec)
	}

	tr := &TrainingRecord{
		TrainingID:      id,
		UserID:          req.UserId,
		ModelDefinition: &modelWithoutContent,
		Training:        req.Training,
		Datastores:      req.Datastores,
		TrainingStatus: &grpc_trainer_v2.TrainingStatus{
			Status:              grpc_trainer_v2.Status_QUEUED,
			SubmissionTimestamp: trainerClient.CurrentTimestampAsString(),
		},
		Metrics:               nil,
		EvaluationMetricsSpec: evaluationMetricsSpec,
	}

	// check queue size
	qHandler := s.queues[gpuType]
	if qHandler == nil {
		qHandler = s.queues["ANY"]
	}

	qSize, err := qHandler.Size()
	if err != nil {
		logr.WithError(err).Warnf("failed to get queue size")
	}
	logGpuTypeQueueSize := fmt.Sprintf("%s_%s", gpuType, "queue_size")
	logr.WithFields(logrus.Fields{
		logGpuTypeQueueSize: qSize,
	})
	logr.Infof("queue %s has %d elements", gpuType, qSize)
	s.metrics.queueSizeGauge.With("gpuType", gpuType).Set(float64(qSize))

	limit := getQueueSizeLimit()
	if limit > 0 && qSize >= limit {
		logr.Infof("queue has too many jobs, rejecting %s", id)
		return nil, gerrf(codes.ResourceExhausted, grpcErrorDesc(errors.New("job could not be queued, queue size exceeds limit")))
	}

	// check if job should be ratelimited
	rateLimited := true
	zone := ""
	if qSize == 0 {
		// ignore possible errors reaching dlaas-ratelimiter
		//rateLimited, zone, _ = s.rateLimitTrainingJob(tr, logr)
		rateLimited = false
	}

	if rateLimited {
		// either queue was not empty or rate-limiting was needed, so send this job to the queue
		logr.Infof("training job %s is rate-limited, adding to queue %s", tr.TrainingID, gpuType)
		enqueueErr := qHandler.Enqueue(id)
		if enqueueErr != nil {
			// store training record with FAILED status
			tr.TrainingStatus.Status = grpc_trainer_v2.Status_FAILED
			tr.TrainingStatus.StatusMessage = fmt.Sprintf("Job was rate-limited and could not be enqueued")
			tr.TrainingStatus.ErrorCode = trainerClient.ErrCodeFailEnqueue
			err := s.repo.Store(tr)
			if err != nil {
				logr.WithError(err).Errorln("Unable to store job with status FAILED")
			}

			// err logged in Enqueue(id)
			return nil, gerrf(codes.Internal, grpcErrorDesc(enqueueErr))
		}

		s.metrics.enqueueJobCounter.Add(1)

		// store training record with QUEUED status
		err := s.repo.Store(tr)
		if err != nil {
			logr.WithError(err).Errorf("Failed to resolve output datastore")
			return nil, gerrf(codes.Internal, grpcErrorDesc(err))
		}
		cl.Observe("stored record in mongo")
	} else {
		logr.Infof("%s queue is empty and job is not rate-limited, sending %s directly to LCM", gpuType, tr.TrainingID)
		logr.Debugf("submitting job to zone %s", zone)

		// set zone in training record, will be stored to mongo in submitJobToLCM
		tr.Zone = zone

		err = s.submitJobToLCM(tr, logr)
		if err != nil {
			// err logged in submitJobToLCM
			return nil, err
		}
		cl.Observe("submitted job to lcm")
	}

	//request is validated, now bump up the counter
	logUserGpuValue := fmt.Sprintf("%s-%s-%v", req.UserId, req.Training.Resources.GpuType, req.Training.Resources.Gpus)

	logUserFrameworkVersionGpuValue := fmt.Sprintf("%s-%s-%s-%s-%v", req.UserId, req.Training.Resources.GpuType, req.ModelDefinition.Framework.Name, req.ModelDefinition.Framework.Version, req.Training.Resources.Gpus)

	logr.WithFields(logrus.Fields{
		"userid_gputype_gpus":           logUserGpuValue,
		"userid_framework_gputype_gpus": logUserFrameworkVersionGpuValue,
	}).Debug(" incrementing userid log")

	s.metrics.trainerUsageCounter.With("userid", req.UserId,
		"gpuType", req.Training.Resources.GpuType,
		"framework", req.ModelDefinition.Framework.Name,
		"version", req.ModelDefinition.Framework.Version).Add(float64(req.Training.Resources.Gpus))

	s.metrics.trainerUsageGauge.With("userid", req.UserId,
		"gpuType", req.Training.Resources.GpuType,
		"framework", req.ModelDefinition.Framework.Name,
		"version", req.ModelDefinition.Framework.Version).Add(float64(req.Training.Resources.Gpus))

	return &grpc_trainer_v2.CreateResponse{TrainingId: id}, nil
}

func (s *trainerService) GetTrainingJob(ctx context.Context, req *grpc_trainer_v2.GetRequest) (*grpc_trainer_v2.GetResponse, error) {
	logr := logger.LocLogger(logWith(req.TrainingId, req.UserId))

	cl := instrumentation.NewCallLogger(ctx, "GetTrainingJob", logr)
	defer cl.Returned()

	tr, err := s.repo.Find(req.TrainingId)
	if err != nil {
		if err == mgo.ErrNotFound {
			return nil, gerrf(codes.NotFound, "Training with id %s not found.", req.TrainingId)
		}
		logr.WithError(err).Errorf("Cannot retrieve training record")
		return nil, err
	}

	cl.Observe("got training job record")

	if tr.UserID != req.UserId {
		msg := fmt.Sprint("User does not have permission to read training data")
		logr.Error(msg)
		return nil, gerrf(codes.PermissionDenied, msg)
	}
	jobb := &grpc_trainer_v2.Job{
		UserId:          tr.UserID,
		JobId:           tr.JobID,
		ModelDefinition: tr.ModelDefinition,
		TrainingId:      tr.TrainingID,
		Training:        tr.Training,
		Status:          tr.TrainingStatus,
		Datastores:      tr.Datastores,
		Metrics:         tr.Metrics,
	}
	return &grpc_trainer_v2.GetResponse{
		Job: jobb,
	}, nil
}

func (s *trainerService) GetTrainingStatusID(ctx context.Context, req *grpc_trainer_v2.GetRequest) (*grpc_trainer_v2.GetStatusIDResponse, error) {
	logr := logger.LocLogger(logWith(req.TrainingId, req.UserId))

	statusID, err := s.repo.FindTrainingStatusID(req.TrainingId)
	if err != nil {
		if err == mgo.ErrNotFound {
			return nil, gerrf(codes.NotFound, "Training with id %s not found.", req.TrainingId)
		}
		logr.WithError(err).Errorf("Cannot retrieve record for training %s", req.TrainingId)
		return nil, err
	}
	return &grpc_trainer_v2.GetStatusIDResponse{
		Status: statusID,
	}, nil
}

func (s *trainerService) UpdateTrainingJob(ctx context.Context, req *grpc_trainer_v2.UpdateRequest) (*grpc_trainer_v2.UpdateResponse, error) {
	logr := logger.LocLogger(logWith(req.TrainingId, req.UserId))
	logr.Debugf("UpdateTrainingJob called for training %s", req.TrainingId)

	return updateTrainingJobPostLock(s, req)
}

// This method contains all the functionality of UpdateTrainingJob, minus the lock on the database.  This enables it to be called
// from within another function, which already has the lock itself (Halt)
func updateTrainingJobPostLock(s *trainerService, req *grpc_trainer_v2.UpdateRequest) (*grpc_trainer_v2.UpdateResponse, error) {
	logr := logger.LocLogger(logWith(req.TrainingId, req.UserId))
	training, err := s.repo.Find(req.TrainingId)
	if err != nil {
		logr.WithError(err).Errorf("Cannot retrieve training '%s'", req.TrainingId)
		return nil, err
	}
	if training == nil {
		// training does not exist
		return nil, gerrf(codes.NotFound, "Training with id %s not found.", req.TrainingId)
	}

	if training.UserID != req.UserId {
		msg := fmt.Sprintf("User %s does not have permission to update training data with id %s.", req.UserId, req.TrainingId)
		logr.Error(msg)
		return nil, gerrf(codes.PermissionDenied, msg)
	}

	ts := training.TrainingStatus
	originalStatus := ts.Status

	// If status is completed/failed/halted and the update is requesting a halt, then do nothing and return error
	if (originalStatus == grpc_trainer_v2.Status_COMPLETED || originalStatus == grpc_trainer_v2.Status_FAILED || originalStatus == grpc_trainer_v2.Status_HALTED) && req.Status == grpc_trainer_v2.Status_HALTED {
		return nil, err
	}

	ts.Status = req.Status
	ts.StatusMessage = req.StatusMessage
	ts.ErrorCode = req.ErrorCode

	nowMillis := trainerClient.CurrentTimestampAsString()

	if req.Status == grpc_trainer_v2.Status_COMPLETED || req.Status == grpc_trainer_v2.Status_FAILED || req.Status == grpc_trainer_v2.Status_HALTED {
		training.Datastores = nil
		ts.CompletionTimestamp = nowMillis
		if req.Timestamp != "" {
			ts.CompletionTimestamp = req.Timestamp
		}
		// erase sensitive data from the db
		training.ModelDefinition.Framework.ImageLocation = nil
	}
	if req.Status == grpc_trainer_v2.Status_DOWNLOADING {
		ts.DownloadStartTimestamp = nowMillis
		if req.Timestamp != "" {
			ts.DownloadStartTimestamp = req.Timestamp
		}
	}
	if req.Status == grpc_trainer_v2.Status_PROCESSING {
		ts.ProcessStartTimestamp = nowMillis
		if req.Timestamp != "" {
			ts.ProcessStartTimestamp = req.Timestamp
		}
	}
	if req.Status == grpc_trainer_v2.Status_STORING {
		ts.StoreStartTimestamp = nowMillis
		if req.Timestamp != "" {
			ts.StoreStartTimestamp = req.Timestamp
		}
	}

	// send monitoring metrics for failed/succeeded jobs
	if req.Status == grpc_trainer_v2.Status_COMPLETED || req.Status == grpc_trainer_v2.Status_FAILED || req.Status == grpc_trainer_v2.Status_HALTED {
		counter := s.metrics.trainingJobSucceededCounter
		if req.Status == grpc_trainer_v2.Status_FAILED {
			errorType := "server"
			if strings.HasPrefix(req.ErrorCode, "C") {
				errorType = "client"
			}

			counter = s.metrics.trainingJobFailedCounter.With("type", errorType, "errorcode", req.ErrorCode)

			s.metrics.trainingJobFailedMetricsBunch.incrementFailedTrainingMetrics(training.ModelDefinition.Framework.Name, training.ModelDefinition.Framework.Version,
				training.Training.GetResources().GpuType, strconv.Itoa(int(training.Training.Resources.Cpus)), strconv.Itoa(int(training.Training.Resources.Gpus)), req.ErrorCode)
			s.metrics.trainingJobFailedGauge.Add(1)
		}
		gpusUsed := training.Training.Resources.Gpus
		if training.Training.Resources.Learners > 1 {
			gpusUsed = training.Training.Resources.Gpus * float32(training.Training.Resources.Learners)
		}

		logGpuTypeDecrementValue := fmt.Sprintf("%s-%v", training.Training.GetResources().GpuType, gpusUsed)
		logr.WithFields(logrus.Fields{
			"gputype_decrement": logGpuTypeDecrementValue,
		}).Debug(" decrementing the gpus")

		logUserGpuValue := fmt.Sprintf("%s-%s-%v", req.UserId, training.Training.GetResources().GpuType, gpusUsed)

		logUserFrameworkVersionGpuValue := fmt.Sprintf("%s-%s-%s-%s-%v", req.UserId, training.Training.GetResources().GpuType, training.ModelDefinition.Framework.Name, training.ModelDefinition.Framework.Version, gpusUsed)

		logr.WithFields(logrus.Fields{
			"userid_gputype_gpus":           logUserGpuValue,
			"userid_framework_gputype_gpus": logUserFrameworkVersionGpuValue,
		}).Debug(" decrementing userid log")

		s.metrics.trainerUsageGauge.With("userid", req.UserId,
			"gpuType", training.Training.GetResources().GpuType,
			"framework", training.ModelDefinition.Framework.Name,
			"version", training.ModelDefinition.Framework.Version).Add(float64(gpusUsed) * -1)

		counter.With("framework", training.ModelDefinition.Framework.Name,
			"version", training.ModelDefinition.Framework.Version,
			"gpus", strconv.Itoa(int(training.Training.Resources.Gpus)),
			"cpus", strconv.Itoa(int(training.Training.Resources.Cpus)),
			"memory", strconv.Itoa(int(training.Training.Resources.Memory))).Add(1)
	}
	err = s.repo.Store(training)
	if err != nil {
		logr.WithError(err).Errorf("Failed updating status of training %s in DB", req.TrainingId)
		return nil, err
	}

	// verify that the training job details have been updated properly
	training, err = s.repo.Find(req.TrainingId)
	if err != nil {
		logr.WithError(err).Errorf("Cannot retrieve training '%s'", req.TrainingId)
		return nil, err
	}
	if training == nil {
		// training does not exist
		return nil, gerrf(codes.NotFound, "Training with id %s not found.", req.TrainingId)
	}
	ts = training.TrainingStatus
	logGpuTypeUsagesValue := fmt.Sprintf("%s-%v", training.Training.GetResources().GpuType, strconv.Itoa(int(training.Training.Resources.Gpus)))
	logFrameworkVersionValue := fmt.Sprintf("%s-%s", training.ModelDefinition.Framework.Name, training.ModelDefinition.Framework.Version)

	logr.WithFields(logrus.Fields{
		logger.LogkeyFrameworkVersion: logFrameworkVersionValue,
		logger.LogkeyGpuType:          training.Training.GetResources().GpuType,
		logger.LogkeyGpuUsage:         logGpuTypeUsagesValue,
		logger.LogkeyErrorCode:        req.ErrorCode,
		"training_status":             ts.Status,
		"training_status_message":     ts.StatusMessage,
	}).Infof("CHECKING metrics for training in updateTrainingJobPostLock")

	// Additionally, store any job state transitions in the job_history DB collection
	// We store a history record if either (1) the status is different, or (2) if this is
	// a PROCESSING->PROCESSING transition, to record the full picture for distributed jobs.
	if req.Status != originalStatus || req.Status == grpc_trainer_v2.Status_PROCESSING {
		timestamp := req.Timestamp
		if req.Timestamp == "" {
			// If timestamp is missing, we may end up storing duplicate events (with different timestamps)
			// in the job_history DB collection. Technically, that shouldn't happen (as we always add a
			// timestamp in controller/jobmonitor when calling UpdateTrainingJob(..))
			logr.Warnf("Timestamp missing in UpdateTrainingJob(..) request, adding current time.")
			timestamp = nowMillis
		}
		e := &JobHistoryEntry{
			TrainingID:    req.TrainingId,
			Timestamp:     timestamp,
			Status:        req.Status,
			StatusMessage: req.StatusMessage,
			ErrorCode:     req.ErrorCode,
		}
		s.jobHistoryRepo.RecordJobStatus(e)
	}

	return &grpc_trainer_v2.UpdateResponse{TrainingId: training.TrainingID}, nil
}

func (s *trainerService) GetAllTrainingsJobs(ctx context.Context, req *grpc_trainer_v2.GetAllRequest) (*grpc_trainer_v2.GetAllResponse, error) {
	logr := logger.LocLogger(logEntry().WithField(logger.LogkeyUserID, req.UserId))
	logr.Debugf("GetAllTrainingsJobs called")

	cl := instrumentation.NewCallLogger(ctx, "GetAllTrainingsJobs", logr)
	defer cl.Returned()

	jobs, err := s.repo.FindAll(req.UserId)
	if err != nil {
		msg := "Failed to retrieve all training jobs"
		logr.WithError(err).Errorf(msg)
		return nil, gerrf(codes.Internal, msg)
	}
	resp := &grpc_trainer_v2.GetAllResponse{
		Jobs: make([]*grpc_trainer_v2.Job, len(jobs)),
	}
	for i, job := range jobs {
		resp.Jobs[i] = &grpc_trainer_v2.Job{
			UserId:          job.UserID,
			JobId:           job.JobID,
			ModelDefinition: job.ModelDefinition,
			TrainingId:      job.TrainingID,
			Training:        job.Training,
			Status:          job.TrainingStatus,
			Datastores:      job.Datastores,
		}
	}
	return resp, nil
}

// cover for depreciated grpc method
func grpcCode(err error) codes.Code {
	if s, ok := status.FromError(err); ok {
		return s.Code()
	}
	return codes.Unknown
}

func (s *trainerService) deleteJobFromTDS(query *tdsService.Query, logr *logger.LocLoggingEntry) error {
	tds, err := s.tdsClient()
	if err != nil {
		logr.WithError(err).Error("Cannot create TDS client")
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute*4)
	defer cancel()

	delResponse, err := tds.Client().DeleteJob(ctx, query)
	if err != nil {
		logr.WithError(err).Error("tds DeleteJob returned error")
		return err
	}
	if !delResponse.Success {
		logr.Warn("tds DeleteJob reported false for success")
	}
	return nil
}

func (s *trainerService) deleteJobFromQueue(trainingID string, gpuType string, logr *logger.LocLoggingEntry) error {
	qHandler := s.queues[gpuType]
	if qHandler == nil {
		qHandler = s.queues["ANY"]
	}

	qerr := qHandler.Lock()
	if qerr != nil {
		logr.WithError(qerr).Errorf("failed to lock %s queue", gpuType)
		return qerr
	}
	defer func() {
		qHandler.Unlock()
	}()

	deleted, err := qHandler.Delete(trainingID)
	if err != nil {
		logr.WithError(err).Errorf("failed to delete job %s from queue %s", trainingID, gpuType)
		return err
	}
	if !deleted {
		logr.Debugf("job %s not found in queue %s", trainingID, gpuType)
	} else {
		logr.Debugf("job %s deleted from queue %s", trainingID, gpuType)
		s.metrics.deleteJobFromQueueCounter.Add(1)
	}
	return nil
}

func (s *trainerService) DeleteTrainingJob(ctx context.Context,
	req *grpc_trainer_v2.DeleteRequest) (*grpc_trainer_v2.DeleteResponse, error) {

	logr := logger.LocLogger(logWith(req.TrainingId, req.UserId))

	cl := instrumentation.NewCallLogger(ctx, "DeleteTrainingJob", logr)
	defer cl.Returned()

	s.metrics.deleteTrainingJobCounter.Add(1)

	readResp, err := s.GetTrainingJob(ctx, &grpc_trainer_v2.GetRequest{
		TrainingId: req.TrainingId,
		UserId:     req.UserId,
	})

	if err != nil {
		logr.WithError(err).Errorf("Failing querying training job")
		return nil, err
	}

	cl.Observe("got training job record")

	// We've noticed that deleting from the TDS can take several minutes, and we don't want to delay this
	// call due to that. This is a temporary workaround until we find out root cause of the TDS slowdowns.
	go func() {
		err = s.deleteJobFromTDS(&tdsService.Query{
			Meta: &tdsService.MetaInfo{
				TrainingId: req.TrainingId,
				UserId:     req.UserId,
			},
		}, logr)
		if err != nil {
			logr.WithError(err).Warn("deleteJobFromTDS returned error")
		}

		cl.Observe("cleaned up job in TDS")
	}()

	var job *grpc_trainer_v2.Job
	if readResp != nil {
		job = readResp.Job

		// delete from queue
		if job.Status.Status == grpc_trainer_v2.Status_QUEUED {
			// if this fails, the queue entry will be cleaned up when the job is pulled
			s.deleteJobFromQueue(job.TrainingId, TransformResourceName(job.Training.Resources.GpuType), logr)
		}

		// Do the LCM cleanup in the background. We noticed this step can take a long time and cause context deadline
		// exceeded errors where there were many concurrent calls to delete a job.
		// As long as we can delete the record from mongo, object store, and the training data service, the user gets a
		// successful status back.
		// LCM failures are silently ignored, so we need alerts when the LCM cleanup fails, and be more proactive
		// in cleaning up stale learners.
		go func() {
			// delete the job if it exists
			lcm, err := s.lcmClient()
			if err != nil {
				logr.WithError(err).Errorln("Cannot create lcm service client")
				return
			}
			defer lcm.Close()

			lcmCtx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			_, err = lcm.Client().KillTrainingJob(lcmCtx, &service.JobKillRequest{
				Name:       job.JobId,
				TrainingId: job.TrainingId,
				UserId:     job.UserId,
			})

			// tolerate "not found" because it just means the job is no longer running
			if err != nil && grpcCode(err) != codes.NotFound {
				logr.WithError(err).Errorf("Failed to kill job '%s'", job.JobId)
				return
			}
			logr.Debugf("Kubernetes job '%s' does not longer exist.", job.JobId)

			cl.Observe("killed job in LCM")
		}()

		// delete model content from data store
		if s.datastore != nil {
			err = s.datastore.DeleteArchive(s.modelsBucket, getModelZipFileName(job.JobId))
			if err != nil {
				logr.WithError(err).Errorf("Error deleting model from object store")
				// log this error, but continue with deleting the training record anyway
			}
			cl.Observe("deleted model from object store")
		}

		// delete from DB
		err = s.repo.Delete(job.TrainingId)
		if err != nil {
			logr.WithError(err).Errorf("Failed to delete training job '%s' from database", job.TrainingId)
			return nil, err
		}
		cl.Observe("deleted model from mongo")

		return &grpc_trainer_v2.DeleteResponse{TrainingId: job.JobId}, nil
	}
	return nil, gerrf(codes.NotFound, "Training with id '%s' not found.", req.TrainingId)
}

func (s *trainerService) HaltTrainingJob(ctx context.Context, req *grpc_trainer_v2.HaltRequest) (*grpc_trainer_v2.HaltResponse, error) {
	logr := logger.LocLogger(logWith(req.TrainingId, req.UserId))
	logr.Debugf("HaltTrainingJob called")

	s.metrics.haltTrainingJobCounter.Add(1)

	readResp, err := s.GetTrainingJob(ctx, &grpc_trainer_v2.GetRequest{
		TrainingId: req.TrainingId,
		UserId:     req.UserId,
	})

	if err != nil {
		logr.WithError(err).Errorf("Failing querying training job")
		return nil, err
	}

	var job *grpc_trainer_v2.Job
	if readResp != nil {
		job = readResp.Job

		// stop the job if exists
		lcm, err := s.lcmClient()
		if err != nil {
			logr.WithError(err).Errorln("Cannot create lcm service client")
			return nil, err
		}
		defer lcm.Close()

		ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
		defer cancel()
		/* Halt in the LCM isn't working, but with mounted cos, the kill should be fine.  All results
		should be in COS either way.  Use Kill instead, which is well tested */
		_, err = lcm.Client().KillTrainingJob(ctx, &service.JobKillRequest{
			Name:       job.JobId,
			TrainingId: job.TrainingId,
			UserId:     job.UserId,
		})

		// tolerate "not found" because it just means the job is no longer running
		if err != nil && grpcCode(err) != codes.NotFound {
			logr.WithError(err).Errorf("Failed to kill job '%s'", job.JobId)
			return nil, err
		}
		logr.Debugf("Kubernetes job '%s' no longer exists.", job.JobId)

		// update the status in mongo
		_, err = updateTrainingJobPostLock(s, &grpc_trainer_v2.UpdateRequest{
			TrainingId:    req.TrainingId,
			UserId:        req.UserId,
			Status:        grpc_trainer_v2.Status_HALTED,
			StatusMessage: "Halted by user",
			ErrorCode:     "0",
		})
		if err != nil {
			logr.WithError(err).Errorln("Unable to update job status to halted")
			return nil, err
		}

		return &grpc_trainer_v2.HaltResponse{TrainingId: job.JobId, UserId: job.UserId, Status: grpc_trainer_v2.Status_HALTED}, nil
	}
	return nil, gerrf(codes.NotFound, "Training with id '%s' not found.", req.TrainingId)
}

func (s *trainerService) ResumeTrainingJob(ctx context.Context, req *grpc_trainer_v2.ResumeRequest) (*grpc_trainer_v2.ResumeResponse, error) {
	logr := logger.LocLogger(logWith(req.TrainingId, req.UserId))
	logr.Debugf("HaltTrainingJob called")
	return nil, gerrf(codes.Unimplemented, "ResumeTrainingJob not implemented yet")
}

func (s *trainerService) GetModelDefinition(req *grpc_trainer_v2.ModelDefinitionRequest, stream grpc_trainer_v2.Trainer_GetModelDefinitionServer) error {
	logr := logger.LocLogger(logWith(req.TrainingId, req.UserId))
	logr.Infof("GetModelDefinition")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	resp, err := s.GetTrainingJob(ctx, &grpc_trainer_v2.GetRequest{
		TrainingId: req.TrainingId,
		UserId:     req.UserId,
	})
	if err != nil {
		logr.WithError(err).Errorf("Failed to read training with id: %s", req.TrainingId)
		return gerrf(codes.Internal, "Failed to read training with id: %s", req.TrainingId)
	}
	if resp == nil || resp.Job == nil {
		return gerrf(codes.NotFound, "Training with id '%s' not found.", req.TrainingId)
	}

	if s.datastore == nil {
		msg := "Operation not supported. Download the model from the result directory"
		logr.Errorf(msg)
		return errors.New(msg)
	}

	// TODO we need to change this to accept a writer to be more efficient
	payload, err := s.datastore.DownloadArchive(s.modelsBucket, getModelZipFileName(req.TrainingId))
	if err != nil {
		logr.WithError(err).Errorf("Downloading model definition archive failed")
	}
	err = stream.Send(&grpc_trainer_v2.ZippedDataChunk{
		Data: payload,
	})
	if err != nil {
		logr.WithError(err).Errorf("Failed to send zipped chunk.")
		return err
	}
	return nil
}

func (s *trainerService) GetTrainedModel(req *grpc_trainer_v2.TrainedModelRequest, stream grpc_trainer_v2.Trainer_GetTrainedModelServer) error {

	logr := logger.LocLogger(logWith(req.TrainingId, req.UserId))
	logr.Infof("GetTrainedModel")

	s.metrics.downloadTrainedModelJobCounter.Add(1)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	resp, err := s.GetTrainingJob(ctx, &grpc_trainer_v2.GetRequest{
		TrainingId: req.TrainingId,
		UserId:     req.UserId,
	})
	if err != nil {
		logr.WithError(err).Errorf("Error reading training with id: %s", req.TrainingId)
		return err
	}
	if resp == nil || resp.Job == nil {
		return gerrf(codes.NotFound, "Training with id '%s' not found.", req.TrainingId)
	}

	var ostore storage.DataStore
	ds := s.getOutputDatastore(resp.Job.Training.OutputData, resp.Job.Datastores)
	ostore, err = storage.CreateDataStore(ds.Type, ds.Connection)
	if err != nil {
		logr.WithError(err).Errorf("Error creating datastore: %v", ds)
		return err
	}
	if err := ostore.Connect(); err != nil {
		logr.WithError(err).Error("Error connect to datastore")
		return err
	}
	defer ostore.Disconnect()

	trainedModelSize, err := ostore.GetTrainedModelSize(fmt.Sprintf("%s/%s", ds.Fields["bucket"], resp.Job.TrainingId),
		resp.Job.Training.Resources.Learners)

	if err != nil {
		logr.WithError(err).Error("Error retrieving trained model size")
		return err
	}
	logr.Debugf("The size of the trained model is %d", trainedModelSize)

	// DP only allows downloads of sizes less than 200MBs
	if trainedModelSize > 200000000 {
		logr.Debugf("Trained model for '%s' exceeded download limit size.", req.TrainingId)
		return gerrf(codes.FailedPrecondition,
			"Trained model exceeded download limit. Download from your cloud storage directly")
	}

	r, w := io.Pipe() // connect I/O without temp space.

	go func() {
		// write to pipe by downloading
		err := ostore.DownloadTrainedModelAsZipStream(fmt.Sprintf("%s/%s", ds.Fields["bucket"], resp.Job.TrainingId),
			resp.Job.Training.Resources.Learners, w)

		if err != nil {
			logr.WithError(err).Error("Downloading trained model failed")
			w.CloseWithError(err)
		}
		if err := w.Close(); err != nil {
			logr.WithError(err).Error("Closing writer failed")
		}
	}()

	reader := bufio.NewReader(r)
	buf := make([]byte, 0, 10*1024)
	for {
		n, err := reader.Read(buf[:cap(buf)])
		buf = buf[:n]
		if n == 0 {
			if err == nil {
				continue
			}
			if err == io.EOF {
				break
			}
			return err
		}
		// process buf
		if err != nil && err != io.EOF {
			logr.WithError(err).Errorf("Downloading trained model failed")
			return err
		}
		err = stream.Send(&grpc_trainer_v2.ZippedDataChunk{
			Data: buf,
		})
		if err != nil {
			logr.WithError(err).Error("Failed to send zipped data chunk")
			return err
		}
	}
	return nil
}

func trainedModelLogRequestToTrainerQuery(req *grpc_trainer_v2.TrainedModelLogRequest, rindex int64, pageSize int32) *tdsService.Query {
	query := &tdsService.Query{
		Meta: &tdsService.MetaInfo{
			TrainingId: req.TrainingId,
			UserId:     req.UserId,
		},
		Pos:      rindex,
		Pagesize: pageSize,
	}
	return query
}

func (s *trainerService) isLearningFinished(req *grpc_trainer_v2.TrainedModelLogRequest) (bool, error) {
	tr, err := s.repo.Find(req.TrainingId)
	if err != nil {
		if err == mgo.ErrNotFound {
			// Maybe it was deleted.  Call it a day without reporting an error
			return true, nil
		}
		return true, err
	}
	statusID := tr.TrainingStatus.Status

	jobCompleted := false
	if statusID == grpc_trainer_v2.Status_COMPLETED ||
		statusID == grpc_trainer_v2.Status_FAILED ||
		statusID == grpc_trainer_v2.Status_HALTED ||
		statusID == grpc_trainer_v2.Status_STORING {
		jobCompleted = true
	}

	return jobCompleted, nil
}

func (s *trainerService) waitUntilJobStart(req *grpc_trainer_v2.TrainedModelLogRequest,
	outStream grpc_trainer_v2.Trainer_GetTrainedModelLogsServer,
	logr *logger.LocLoggingEntry) error {

	startTime := time.Now()
	lastReportTime := time.Now()
	if req.Follow == true {
		for {
			tr, err := s.repo.Find(req.TrainingId)
			if err != nil {
				return err
			}
			statusID := tr.TrainingStatus.Status
			if !(statusID == grpc_trainer_v2.Status_NOT_STARTED ||
				statusID == grpc_trainer_v2.Status_QUEUED ||
				statusID == grpc_trainer_v2.Status_PENDING) {
				break
			}
			duration := time.Now().Sub(startTime)
			if duration > time.Minute*10 {
				err := errors.New(
					"gave up waiting for job to start when attempting to retrieve learner logs")
				logr.WithError(err).Debugf("gave up waiting")
				return err
			}
			durationSinceLastReport := time.Now().Sub(lastReportTime)
			if durationSinceLastReport.Seconds() == 15 {
				msg := fmt.Sprintf(
					"Waiting for training to start for log follow: %f minutes",
					duration.Minutes())
				logr.Debugf("%s", msg)
				errSend := outStream.Send(&grpc_trainer_v2.ByteStreamResponse{Data: []byte(msg)})
				if errSend != nil {
					logr.WithError(errSend).Errorf("cannot report status to user")
				}
				lastReportTime = time.Now()
			}

			time.Sleep(time.Second * 2)
		}
	}

	return nil
}

func (s *trainerService) GetTrainedModelLogs(req *grpc_trainer_v2.TrainedModelLogRequest,
	outStream grpc_trainer_v2.Trainer_GetTrainedModelLogsServer) error {

	logr := logger.LocLogger(logWith(req.TrainingId, req.UserId))

	//noinspection GoBoolExpressions
	dlogr := debugLogger(logr.Logger, debugLogsMode)

	dlogr.Debug("entry")

	err := s.waitUntilJobStart(req, outStream, logr)
	if err != nil {
		return err
	}

	tds, err := s.tdsClient()
	if err != nil {
		logr.WithError(err).Error("Cannot create TDS client")
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute*4)
	defer cancel()

	var rindex int64 = 1

	for {
		// TODO: Create query from old request
		query := trainedModelLogRequestToTrainerQuery(req, rindex, oldEndpointInternalPageSize)

		dlogr.Debugf("Query to send to training-data: %+v", query)

		inStream, err := tds.Client().GetLogs(ctx, query)
		if err != nil {
			logr.WithError(err).Error("training data service GetLogs seems to have failed")
			return err
		}

		nRecordsFound := 0
		for {
			dlogr.Debugf("inStream.Recv()")
			chunk, err := inStream.Recv()
			if err == io.EOF {
				dlogr.Debug("eof")
				break
			}
			if err != nil {
				logr.WithError(err).Errorf("cannot read trained model log")
				return fmt.Errorf("cannot read trained model log: %v", err)
			}
			dlogr.Debugf("sending line: %d", chunk.Meta.Rindex)
			errSend := outStream.Send(&grpc_trainer_v2.ByteStreamResponse{Data: []byte(chunk.Line)})
			if errSend != nil {
				logr.WithError(errSend).Errorf("cannot send trained model log")
				return fmt.Errorf("cannot send trained model log: %v", err)
			}
			rindex++
			nRecordsFound++
			dlogr.Debugf("sent without error")
		}
		if nRecordsFound == 0 {
			if req.Follow == false {
				break
			}
			isDone, err := s.isLearningFinished(req)
			if err != nil {
				logr.WithError(err).Errorf("Can not get trainer status")
				return err
			}
			if isDone {
				break
			}

			time.Sleep(time.Second * 2)
		}
	}
	dlogr.Debug("exit with nil return")
	return nil
}

func marshalQuerySearchType(st grpc_trainer_v2.Query_SearchType) tdsService.Query_SearchType {
	searchType := tdsService.Query_TERM

	switch st {
	case grpc_trainer_v2.Query_TERM:
		searchType = tdsService.Query_TERM
		break
	case grpc_trainer_v2.Query_NESTED:
		searchType = tdsService.Query_NESTED
		break
	case grpc_trainer_v2.Query_MATCH:
		searchType = tdsService.Query_MATCH
		break
	case grpc_trainer_v2.Query_ALL:
		searchType = tdsService.Query_ALL
		break
	}
	return searchType
}

func marshalTDSQueryToTrainerQuery(in *grpc_trainer_v2.Query) *tdsService.Query {
	query := &tdsService.Query{
		Meta: &tdsService.MetaInfo{
			TrainingId: in.Meta.TrainingId,
			UserId:     in.Meta.UserId,
			Time:       in.Meta.Time,
			Rindex:     in.Meta.Rindex,
			Subid:      in.Meta.Subid,
		},
		Pos:        in.Pos,
		Pagesize:   in.Pagesize,
		Since:      in.Since,
		SearchType: marshalQuerySearchType(in.SearchType),
	}
	return query
}

func (s *trainerService) GetTrainingLogs(in *grpc_trainer_v2.Query,
	outStream grpc_trainer_v2.Trainer_GetTrainingLogsServer) error {

	logr := logger.LocLogger(logWith(in.Meta.TrainingId, in.Meta.UserId))

	//noinspection GoBoolExpressions
	dlogr := debugLogger(logr.Logger, debugLogsMode)

	dlogr.Debug("entry")

	tds, err := s.tdsClient()
	if err != nil {
		logr.WithError(err).Error("Cannot create TDS client")
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute*4)
	defer cancel()

	dlogr.Debugf("Query to send from client: %+v", in)

	query := marshalTDSQueryToTrainerQuery(in)

	dlogr.Debugf("Query to send to training-data: %+v", query)

	inStream, err := tds.Client().GetLogs(ctx, query)

	if err != nil {
		logr.WithError(err).Error("training data service GetLogs seems to have failed")
		return err
	}

	for {
		dlogr.Debugf("inStream.Recv()")
		chunk, err := inStream.Recv()
		if err == io.EOF {
			dlogr.Debug("eof")
			break
		}
		if err != nil {
			logr.WithError(err).Errorf("cannot read trained model log")
			return fmt.Errorf("cannot read trained model log: %v", err)
		}
		dlogr.Debugf("sending line: %d", chunk.Meta.Rindex)
		errSend := outStream.Send(&grpc_trainer_v2.LogLine{
			Meta: &grpc_trainer_v2.MetaInfo{
				TrainingId: chunk.Meta.TrainingId,
				UserId:     chunk.Meta.UserId,
				Time:       chunk.Meta.Time,
				Rindex:     chunk.Meta.Rindex,
				Subid:      chunk.Meta.Subid,
			},
			Line: chunk.Line,
		})
		if errSend != nil {
			logr.WithError(errSend).Errorf("cannot send trained model log")
			return fmt.Errorf("cannot send trained model log: %v", err)
		}
		dlogr.Debugf("sent without error")
	}
	dlogr.Debug("exit with nil return")
	return nil
}

func marshalTDSDataType2TrainerDataType(dt tdsService.Any_DataType) grpc_trainer_v2.Any_DataType {
	dataType := grpc_trainer_v2.Any_STRING

	switch dt {
	case tdsService.Any_STRING:
		dataType = grpc_trainer_v2.Any_STRING
		break
	case tdsService.Any_JSONSTRING:
		dataType = grpc_trainer_v2.Any_JSONSTRING
		break
	case tdsService.Any_INT:
		dataType = grpc_trainer_v2.Any_INT
		break
	case tdsService.Any_FLOAT:
		dataType = grpc_trainer_v2.Any_FLOAT
		break
	}
	return dataType
}

func marshalTDSMapToTrainerMap(tdsMap map[string]*tdsService.Any) map[string]*grpc_trainer_v2.Any {
	grpcMap := make(map[string]*grpc_trainer_v2.Any)
	for k, v := range tdsMap {
		trainerDT := marshalTDSDataType2TrainerDataType(v.Type)
		grpcMap[k] = &grpc_trainer_v2.Any{Type: trainerDT, Value: v.Value}
	}
	return grpcMap
}

func (s *trainerService) GetTrainingEMetrics(in *grpc_trainer_v2.Query,
	outStream grpc_trainer_v2.Trainer_GetTrainingEMetricsServer) error {

	logr := logger.LocLogger(logWith(in.Meta.TrainingId, in.Meta.UserId))
	tds, err := s.tdsClient()
	if err != nil {
		logr.WithError(err).Error("Cannot create TDS client")
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute*4)
	defer cancel()

	query := marshalTDSQueryToTrainerQuery(in)

	inStream, err := tds.Client().GetEMetrics(ctx, query)

	for {
		chunk, err := inStream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			logr.WithError(err).Errorf("cannot read trained model log")
			return fmt.Errorf("cannot read trained model log: %v", err)
		}
		errSend := outStream.Send(&grpc_trainer_v2.EMetrics{
			Meta: &grpc_trainer_v2.MetaInfo{
				TrainingId: chunk.Meta.TrainingId,
				UserId:     chunk.Meta.UserId,
				Time:       chunk.Meta.Time,
				Rindex:     chunk.Meta.Rindex,
				Subid:      chunk.Meta.Subid,
			},
			Grouplabel: chunk.Grouplabel,
			Etimes:     marshalTDSMapToTrainerMap(chunk.Etimes),
			Values:     marshalTDSMapToTrainerMap(chunk.Values),
		})
		if errSend != nil {
			logr.WithError(errSend).Errorf("cannot send trained model log")
			return fmt.Errorf("cannot send trained model log: %v", err)
		}
	}
	return nil
}

func (s *trainerService) GetVersions(ctx context.Context, req *grpc_trainer_v2.GetVersionsRequest) (*grpc_trainer_v2.Frameworks, error) {
	//call the frameworks.go and then getAllVersions for the frameworks
	//Return response from getAll Versions
	frameworks, err := getExternalVersions()
	if err != nil {
		return nil, err
	}
	return &frameworks, nil
}

func (s *trainerService) validateRequest(log *logrus.Entry, req *grpc_trainer_v2.CreateRequest) error {
	if req.UserId == "" {
		return s.failCreateRequest("UserId is nil", req, log)
	}

	// validate model definition object

	m := req.ModelDefinition
	if m == nil {
		return s.failCreateRequest("Model definition is not set", req, log)
	}
	if m.Name == "" {
		return s.failCreateRequest("Model definition name is not set", req, log)
	}
	if m.Framework == nil {
		return s.failCreateRequest("Framework is not set", req, log)
	}
	if m.Framework.Name == "" {
		return s.failCreateRequest("Framework name is not set", req, log)
	}

	if m.Framework.Version == "" {
		return s.failCreateRequest("Framework version is not set", req, log)
	}

	// custom image check
	if m.Framework.ImageLocation == nil {
		if ok, msg := validateFrameworks(m.Framework); !ok {
			return s.failCreateRequest(msg, req, log)
		}
	}

	if len(m.Content) == 0 {
		return s.failCreateRequest("Model definition content is not set", req, log)
	}
	// Initiate a zip reader which does checks for whether the set of bytes represents a valid zip
	// Without this check, an invalid zip causes S301 during the start of training when we try to unzip it
	if _, err := zip.NewReader(bytes.NewReader(m.Content), int64(len(m.Content))); err != nil {
		return s.failCreateRequest("Model zip is invalid: "+err.Error(), req, log)
	}

	// validate Training object

	t := req.Training
	if t == nil {
		return s.failCreateRequest("Training is not set", req, log)
	}
	if t.Command == "" {
		return s.failCreateRequest("Training command is not set", req, log)
	}
	if t.InputData == nil || len(t.InputData) == 0 {
		return s.failCreateRequest("Training input data is not set", req, log)
	}
	if len(t.InputData) > 1 {
		return s.failCreateRequest("Training input data can only contain one id", req, log)
	}
	if s.datastore != nil { // Output data is only optional if an internal dlaas OS is set
		if t.OutputData != nil && len(t.OutputData) > 1 {
			return s.failCreateRequest("Training output data can only contain one id", req, log)
		}
	} else {
		if t.OutputData == nil || len(t.OutputData) == 0 {
			return s.failCreateRequest("Training output data is not set", req, log)
		}
		if len(t.OutputData) > 1 {
			return s.failCreateRequest("Training output data can only contain one id", req, log)
		}
	}

	// validate datastores

	ds := req.Datastores
	if ds == nil {
		return s.failCreateRequest("Data stores is not set", req, log)
	}
	if len(ds) == 0 {
		return s.failCreateRequest("Data stores is empty", req, log)
	}

	for _, name := range t.InputData {
		ds := findDatastore(name, req.Datastores)
		if ds == nil {
			return s.failCreateRequest(fmt.Sprintf("Training input data reference '%s' does not reference an existing datastore id.", name), req, log)
		}
		if err := s.validateDatastore(ds, req, log); err != nil {
			return err
		}
	}

	if len(t.OutputData) > 0 {
		for _, name := range t.OutputData {
			ds := findDatastore(name, req.Datastores)
			if ds == nil {
				return s.failCreateRequest(fmt.Sprintf("Training output data reference '%s' does not reference an existing datastore id.", name), req, log)
			}
			if err := s.validateDatastore(ds, req, log); err != nil {
				return err
			}
		}
	}
	return nil
}

func findDatastore(id string, ds []*grpc_trainer_v2.Datastore) *grpc_trainer_v2.Datastore {
	for _, v := range ds {
		if v.Id == id {
			return v
		}
	}
	return nil
}

func (s *trainerService) failCreateRequest(msg string, req *grpc_trainer_v2.CreateRequest, log *logrus.Entry) error {
	return s.failCreateRequestWithCode(trainerClient.ErrInvalidManifestFile, msg, req, log)
}

func (s *trainerService) failCreateRequestWithCode(errorCode string, msg string, req *grpc_trainer_v2.CreateRequest, log *logrus.Entry) error {
	log.Errorf("Failed to validate CreateRequest: %s", msg)

	// send error event as monitoring metric
	trainingJobFailedMetrics := map[string]string{"framework": "", "version": "", "gpuType": "", "cpus": "", "gpus": "", "errorcode": errorCode}
	counter := s.metrics.trainingJobFailedCounter.With("type", "client", "errorcode", errorCode)

	if req.ModelDefinition != nil && req.ModelDefinition.Framework != nil {
		counter = counter.With("framework", req.ModelDefinition.Framework.Name, "version", req.ModelDefinition.Framework.Version)
		trainingJobFailedMetrics["framework"] = req.ModelDefinition.Framework.Name
		trainingJobFailedMetrics["version"] = req.ModelDefinition.Framework.Version

		logFrameworkErrorsValue := fmt.Sprintf("%s-%s-%s", req.ModelDefinition.Framework.Name, "client", errorCode)
		log.WithFields(logrus.Fields{
			"framework_errors": logFrameworkErrorsValue,
		})
	}

	if req.Training != nil && req.Training.Resources != nil {
		counter = counter.With("gpus", strconv.Itoa(int(req.Training.Resources.Gpus)),
			"cpus", strconv.Itoa(int(req.Training.Resources.Cpus)),
			"memory", strconv.Itoa(int(req.Training.Resources.Memory)))
		trainingJobFailedMetrics["gpuType"] = req.Training.Resources.GpuType
		trainingJobFailedMetrics["cpus"] = strconv.Itoa(int(req.Training.Resources.Cpus))
		trainingJobFailedMetrics["gpus"] = strconv.Itoa(int(req.Training.Resources.Gpus))
	}

	log.Debug("Metrics for failed training jobs framework")
	counter.Add(1)
	s.metrics.trainingJobFailedMetricsBunch.incrementFailedTrainingMetrics(trainingJobFailedMetrics["framework"], trainingJobFailedMetrics["version"], trainingJobFailedMetrics["gpuType"],
		trainingJobFailedMetrics["cpus"], trainingJobFailedMetrics["gpus"], trainingJobFailedMetrics["errorcode"])

	return gerrf(codes.InvalidArgument, msg)
}

func (s *trainerService) validateDatastore(ds *grpc_trainer_v2.Datastore, req *grpc_trainer_v2.CreateRequest, log *logrus.Entry) error {

	if ds == nil {
		return s.failCreateRequest("Data store is not set", req, log)
	}
	if ds.Id == "" {
		return s.failCreateRequest("Data store id is not set", req, log)
	}
	if ds.Connection == nil || len(ds.Connection) == 0 {
		return s.failCreateRequest("Data store connection info not set", req, log)
	}
	if ds.Fields == nil || len(ds.Fields) == 0 {
		return s.failCreateRequest("Data store bucket is not set", req, log)
	}

	ostore, err := storage.CreateDataStore(ds.Type, ds.Connection)
	if err != nil {
		log.Errorf("Validation failed: %s", err.Error())
		return s.failCreateRequestWithCode(trainerClient.ErrInvalidCredentials,
			fmt.Sprintf("Data store authentication information for id '%s' incorrect or there is a connection problem", ds.Id), req, log)
	}

	if err := ostore.Connect(); err != nil {
		log.Errorf("Validation failed: %s", err.Error())
		return s.failCreateRequestWithCode(trainerClient.ErrInvalidCredentials,
			fmt.Sprintf("Data store authentication information for id '%s' incorrect or there is a connection problem", ds.Id), req, log)
	}

	// validate bucket (or container as it is called in Swift)
	bucket := ds.Fields["bucket"]
	if bucket != "" && strings.ToLower(bucket) != noResultBucketTag {
		exists, err := ostore.ContainerExists(bucket)
		if !exists || err != nil {
			return s.failCreateRequestWithCode(trainerClient.ErrInvalidCredentials,
				fmt.Sprintf("Data store bucket '%s' for data store id '%s' incorrect, there may be a connection problem or credentials do not allow access to the bucket", bucket, ds.Id), req, log)
		}
	}
	return nil
}

// lcmClient established a connection if the trainerService has nothing existing cached
func (s *trainerService) lcmClient() (client.LcmClient, error) {
	if s.lcm == nil {
		return client.NewLcm(nil)
	}
	return s.lcm, nil
}

func (s *trainerService) tdsClient() (tdsClient.TrainingDataClient, error) {
	if s.tds == nil {
		address := fmt.Sprintf("%s.%s.svc.cluster.local:80", config.GetTDSServiceName(), config.GetPodNamespace())
		tds, err := tdsClient.NewTrainingDataClientWithAddress(address)
		if err != nil {
			return nil, err
		}
		s.tds = tds
	}
	return s.tds, nil
}

func (s *trainerService) rlClient() (rlClient.RatelimiterClient, error) {
	if s.ratelimiter == nil {
		address := fmt.Sprintf("%s.%s.svc.cluster.local:80", config.GetRatelimiterServiceName(), config.GetPodNamespace())
		ratelimiter, err := rlClient.NewRatelimiterClientWithAddress(address)
		if err != nil {
			return nil, err
		}
		s.ratelimiter = ratelimiter
	}
	return s.ratelimiter, nil
}

func (s *trainerService) createJobConfig(tr *TrainingRecord) (*service.JobDeploymentRequest, error) {
	logr := logger.LocLogger(logWith(tr.TrainingID, tr.UserID))

	// training data/results - assume only one training input and output data at this point
	trainingData := findDatastore(tr.Training.InputData[0], tr.Datastores)
	trainingResults := s.getOutputDatastore(tr.Training.OutputData, tr.Datastores)

	// Environment variables
	envvars := make(map[string]string)

	// Fetching data from user's Object Store with following info
	envvars["DATA_STORE_TYPE"] = trainingData.Type
	envvars["DATA_STORE_AUTHURL"] = trainingData.Connection["auth_url"]
	if trainingData.Connection["project_id"] != "" {
		envvars["DATA_STORE_PROJECTID"] = trainingData.Connection["project_id"]
	}
	if trainingData.Connection["type"] != "" {
		envvars["DATA_STORE_TYPE"] = trainingData.Connection["type"]
	}
	if trainingData.Connection["user_name"] != "" {
		envvars["DATA_STORE_USERNAME"] = trainingData.Connection["user_name"]
	}
	if trainingData.Connection["password"] != "" {
		envvars["DATA_STORE_APIKEY"] = trainingData.Connection["password"]
	}
	if trainingData.Connection["domain_name"] != "" {
		envvars["DATA_STORE_DOMAINNAME"] = trainingData.Connection["domain_name"]
	}
	if trainingData.Connection["region"] != "" {
		envvars["DATA_STORE_REGION"] = trainingData.Connection["region"]
	}
	for k, v := range trainingData.Fields {
		if len(trainingData.Fields) == 1 {
			envvars["DATA_STORE_OBJECTID"] = v
			envvars["DATA_DIR"] = v
		}
		envvars["DATA_STORE_OBJECTID_"+k] = v
		envvars["DATA_DIR_"+k] = v
	}

	// Fetch model from user's object store (only relevant when not using mount_cos)
	osConf := config.GetDataStoreConfig()
	envvars["MODEL_STORE_TYPE"] = trainingResults.Type
	envvars["MODEL_STORE_USERNAME"] = trainingResults.Connection["user_name"]
	envvars["MODEL_STORE_APIKEY"] = trainingResults.Connection["password"]
	envvars["MODEL_STORE_AUTHURL"] = trainingResults.Connection["auth_url"]
	if trainingResults.Connection[storage.StorageType] != "" {
		envvars["MODEL_STORE_TYPE"] = envvars["RESULT_STORE_TYPE"]
	}

	// only needed for Bluemix objectstore
	if val, ok := osConf[storage.DomainKey]; ok {
		envvars["MODEL_STORE_DOMAINNAME"] = val
	}
	if val, ok := osConf[storage.RegionKey]; ok {
		envvars["MODEL_STORE_REGION"] = val
	}
	if val, ok := osConf[storage.DomainKey]; ok {
		envvars["MODEL_STORE_PROJECTID"] = val
	}
	envvars["MODEL_STORE_OBJECTID"] = tr.ModelDefinition.Location

	// "Storing trained model in DLaaS Object Store with following info:"
	envvars["RESULT_STORE_TYPE"] = trainingResults.Type
	envvars["RESULT_STORE_USERNAME"] = trainingResults.Connection["user_name"]
	envvars["RESULT_STORE_APIKEY"] = trainingResults.Connection["password"]
	envvars["RESULT_STORE_AUTHURL"] = trainingResults.Connection["auth_url"]
	if trainingResults.Connection[storage.StorageType] != "" {
		envvars["RESULT_STORE_TYPE"] = trainingResults.Connection[storage.StorageType]
	}
	// only needed for Bluemix objectstore
	if trainingResults.Connection["domain_name"] != "" {
		envvars["RESULT_STORE_DOMAINNAME"] = trainingResults.Connection["domain_name"]
	}
	if trainingResults.Connection["region"] != "" {
		envvars["RESULT_STORE_REGION"] = trainingResults.Connection["region"]
	}
	if trainingResults.Connection["project_id"] != "" {
		envvars["RESULT_STORE_PROJECTID"] = trainingResults.Connection["project_id"]
	}
	if strings.ToLower(trainingResults.Fields["bucket"]) == noResultBucketTag {
		envvars["RESULT_STORE_OBJECTID"] = noResultBucketTag
	} else {
		envvars["RESULT_STORE_OBJECTID"] = fmt.Sprintf("%s/%s", trainingResults.Fields["bucket"], tr.TrainingID)
	}
	// Storing model in container at
	envvars["MODEL_DIR"] = "/model-code"

	// Storing trained model at
	envvars["RESULT_DIR"] = trainingResults.Fields["bucket"]

	// TODO: This is pointing to currently where the logs are put, but should be redefined per nfs log mount proposal.
	// (by the time it gets to the learners/log-collectors, it will be "/job/logs", at the time of this writing.)
	envvars["LOG_DIR"] = "/logs"

	re := regexp.MustCompile(`\r?\n`)
	input := re.ReplaceAllString(fmt.Sprint(tr.Training.Command), " ")

	envvars["TRAINING_COMMAND"] = input

	envvars["TRAINING_ID"] = tr.TrainingID

	envvars["GPU_COUNT"] = strconv.FormatFloat(float64(tr.Training.Resources.Gpus), 'f', 6, 64)

	envvars["SCHED_POLICY"] = strings.ToLower(tr.Training.Resources.Schedpolicy)

	// tag to use to lookup learner image to use; this is a Docker image tag
	if tr.ModelDefinition.Framework.ImageTag != "" {
		envvars["DLAAS_LEARNER_IMAGE_TAG"] = tr.ModelDefinition.Framework.ImageTag
	}

	// envvar for profile
	if tr.Training.Profiling {
		envvars["DLAAS_PROFILING"] = "true"
	}

	// labels
	labels := make(map[string]string)
	labels["training_id"] = tr.TrainingID
	labels["user_id"] = tr.UserID
	labels["gpu_type"] = tr.Training.Resources.GpuType
	labels["deploy_zone"] = tr.Zone

	u4, err := uuid.NewV4()
	if err != nil {
		return nil, err
	}

	logr.Debugf("Training job env vars: %v", envvars)

	job := &service.JobDeploymentRequest{
		Name:                  u4.String(),
		Resources:             getResourceRequirements(tr.Training),
		EnvVars:               envvars,
		Labels:                labels,
		UserId:                tr.UserID,
		TrainingId:            tr.TrainingID,
		Framework:             tr.ModelDefinition.Framework.Name,
		Version:               tr.ModelDefinition.Framework.Version,
		ImageTag:              tr.ModelDefinition.Framework.ImageTag,
		ImageLocation:         parseImageLocation(tr),
		EvaluationMetricsSpec: tr.EvaluationMetricsSpec,
	}

	return job, nil
}

func parseImageLocation(tr *TrainingRecord) *service.ImageLocation {
	tril := tr.ModelDefinition.Framework.ImageLocation
	var il (*service.ImageLocation)
	if tril != nil {
		il = &service.ImageLocation{
			Registry:    tril.Registry,
			Namespace:   tril.Namespace,
			AccessToken: tril.AccessToken,
			Email:       tril.Email,
		}
	}
	return il
}

func setDefaultResourceRequirements(t *grpc_trainer_v2.Training) {
	if t.Resources.Gpus == 0 {
		t.Resources.GpuType = "CPU"
	}
	if t.Resources.Cpus == 0 {
		t.Resources.Cpus = 5.0
	}
	if t.Resources.Memory == 0 {
		t.Resources.Memory = 12
		t.Resources.MemoryUnit = grpc_trainer_v2.SizeUnit_GiB
	}
	if t.Resources.Schedpolicy == "" || strings.ToLower(t.Resources.Schedpolicy) != "spread" {
		t.Resources.Schedpolicy = "dense"
	}
	if TransformResourceName(t.Resources.GpuType) == "CPU" {
		t.Resources.Gpus = 0.0
	}
	if t.Resources.GpuType == "" {
		t.Resources.GpuType = "nvidia-TeslaK80"
	}

}

func getResourceRequirements(t *grpc_trainer_v2.Training) *service.ResourceRequirements {
	return &service.ResourceRequirements{
		Cpus:        float64(t.Resources.Cpus),
		Gpus:        float64(t.Resources.Gpus),
		Memory:      float64(t.Resources.Memory),
		MemoryUnit:  service.ResourceRequirements_MemoryUnit(service.ResourceRequirements_MemoryUnit_value[t.Resources.MemoryUnit.String()]),
		Storage:     float64(t.Resources.Storage),
		StorageUnit: service.ResourceRequirements_MemoryUnit(service.ResourceRequirements_MemoryUnit_value[t.Resources.StorageUnit.String()]),
		Learners:    t.Resources.Learners,
		GpuType:     t.Resources.GpuType,
	}
}

// getOutputDatastore retrieves the output data store or return the internal datastore if none has been defined
func (s *trainerService) getOutputDatastore(outputData []string, datastores []*grpc_trainer_v2.Datastore) *grpc_trainer_v2.Datastore {
	var ds *grpc_trainer_v2.Datastore
	if len(outputData) > 0 {
		ds = findDatastore(outputData[0], datastores) // we assume there is only one output data at this point b/c the underlying system does not support more
	}
	if ds == nil {
		ds = &grpc_trainer_v2.Datastore{
			Id:         internalObjectStoreID,
			Type:       config.GetDataStoreType(),
			Connection: config.GetDataStoreConfig(),
			Fields:     map[string]string{"bucket": s.trainedModelsBucket},
		}
	}
	return ds
}

// getOutputStoreForService is a wrapper function to make the logic in trainerService.getOutputDatastore available for testing
func getOutputStoreForService(s *trainerService, outputData []string, datastores []*grpc_trainer_v2.Datastore) *grpc_trainer_v2.Datastore {
	return s.getOutputDatastore(outputData, datastores)
}

func getModelsBucket() string {
	if viper.IsSet(modelsBucketKey) {
		return viper.GetString(modelsBucketKey)
	}
	return ""
}

func getTrainedModelsBucket() string {
	if viper.IsSet(trainedModelsBucketKey) {
		return viper.GetString(trainedModelsBucketKey)
	}
	return defaultTrainedModelsBucket
}

func getModelZipFileName(trainingID string) string {
	return fmt.Sprintf("%s.zip", trainingID)
}

func (s *trainerService) submitJobToLCM(tr *TrainingRecord, logr *logger.LocLoggingEntry) error {
	jobConfig, err := s.createJobConfig(tr)
	if err != nil {
		logr.WithError(err).Errorf("Failed to create job config")
		return gerrf(codes.Internal, grpcErrorDesc(err))
	}

	// store training record with PENDING status
	tr.TrainingStatus.Status = grpc_trainer_v2.Status_PENDING
	err = s.repo.Store(tr)
	if err != nil {
		logr.WithError(err).Errorf("Failed to resolve output datastore")
		return gerrf(codes.Internal, grpcErrorDesc(err))
	}

	lcm, err := s.lcmClient()
	if err != nil {
		logr.WithError(err).Errorf("Cannot create LCM service client")
		return gerrf(codes.Internal, grpcErrorDesc(err))
	}
	defer lcm.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	_, err = lcm.Client().DeployTrainingJob(ctx, jobConfig)
	if err != nil {
		logr.WithError(err).Errorf("Cannot deploy training job with id %s", tr.TrainingID)
		return gerrf(codes.Internal, grpcErrorDesc(err))
	}

	logr.Printf("training job %s submitted to lcm", tr.TrainingID)

	// capture the gpu usage when the job is submitted to LCM
	gpusUsed := tr.Training.Resources.Gpus
	if tr.Training.Resources.Learners > 1 {
		gpusUsed = tr.Training.Resources.Gpus * float32(tr.Training.Resources.Learners)
	}
	// log the gpu usages requested by user
	logGpuTypeIncrementValue := fmt.Sprintf("%s-%v", tr.Training.Resources.GpuType, gpusUsed)
	logr.WithFields(logrus.Fields{
		"gputype_increment": logGpuTypeIncrementValue,
	}).Debug(" incrementing the gpus")

	// increment the counter
	s.metrics.clusterWideGPUUsageCounter.With("gpuType", tr.Training.Resources.GpuType, "gpus", strconv.Itoa(int(gpusUsed))).Add(1)

	return nil
}

// rateLimitTrainingJob makes a grpc call to the dlaas-ratelimiter service and returns
// true if the job should be ratelimited, and an error if the call to ratelimiter fails
func (s *trainerService) rateLimitTrainingJob(trainingRecord *TrainingRecord, logr *logger.LocLoggingEntry) (bool, string, error) {
	// compute GPUs requested
	gpuType := trainingRecord.Training.Resources.GpuType
	gpus := int64(trainingRecord.Training.Resources.Gpus)
	if trainingRecord.Training.Resources.Learners > 1 {
		// trainingRecord.Training.Resources.Gpus is GPUs used per learner
		gpus = int64(trainingRecord.Training.Resources.Gpus * float32(trainingRecord.Training.Resources.Learners))
	}
	cpus := trainingRecord.Training.Resources.Cpus

	logr.Debugf("ratelimit check for %d %s GPUs, %f CPUs", gpus, gpuType, cpus)

	// query ratelimiter service
	// if the grpc call fails, allow the job so we do not block trainings when ratelimiter is down
	rlc, err := s.rlClient()
	if err != nil {
		logr.WithError(err).Infof("Cannot create ratelimiter service client, job will not be ratelimited")
		s.ratelimiter = nil // clear ratelimiter client to attempt to reconnect on next call
		return false, "", err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()

	request := &rlService.RatelimitRequest{
		Gpus:       gpus,
		GpuType:    gpuType,
		TrainingId: trainingRecord.TrainingID,
		UserId:     trainingRecord.UserID,
		Cpus:       cpus,
	}
	rlResponse, rlErr := rlc.Client().JobAllowed(ctx, request)
	if rlErr != nil {
		logr.WithError(rlErr).Info("ratelimit check failed, job will be allowed")
		s.ratelimiter = nil // clear ratelimiter client to attempt to reconnect on next call
		return false, "", rlErr
	}

	rateLimit := !rlResponse.Allowed
	zone := rlResponse.Zone

	logr.Debugf("result of rate-limiting for job %s is %t in zone %s; requested %d GPUs of type %s, %f CPUs",
		trainingRecord.TrainingID, rateLimit, zone, gpus, gpuType, cpus)
	if rateLimit {
		s.metrics.rateLimitTrainingJobCounter.Add(1)
	}

	return rateLimit, zone, nil
}

func getGpuLimitQuerySize() int {
	return viper.GetInt(gpuLimitsQuerySizeKey)
}

func getQueueSizeLimit() int {
	return viper.GetInt(queueSizeLimitKey)
}

//getAllResourceTypes returns all possible GPU types (K80, P100, V100)
func getAllResourceTypes() []string {
	types := []string{"nvidia-TeslaK80", "nvidia-TeslaP100", "nvidia-TeslaV100", "CPU"}
	return types
}

//getGpuLimitByType returns the resource limit if it is defined, or returns 0 if not
func getGpuLimitByType(gpuType string) int64 {
	limit := int64(0)
	allLimits := strings.Split(viper.GetString(gpuLimitsKey), ",")
	for _, l := range allLimits {
		if TransformResourceName(strings.Split(l, "=")[0]) == TransformResourceName(gpuType) {
			lim, err := strconv.ParseInt(strings.Split(l, "=")[1], 10, 0)
			if err == nil {
				limit = lim
			}
			break
		}
	}
	return limit
}

//getGpuMapFromConfig returns a map using gpu name as key, and limit as value
func getGpuMapFromConfig() (map[string]int64, error) {
	gpuMap := make(map[string]int64)
	allGpuAndLimits := strings.Split(viper.GetString(gpuLimitsKey), ",")
	for _, gpuAndLimit := range allGpuAndLimits {
		gpuType := strings.Split(gpuAndLimit, "=")[0]
		limit, err := strconv.ParseInt(strings.Split(gpuAndLimit, "=")[1], 10, 0)
		if err != nil {
			return nil, err
		}
		gpuMap[TransformResourceName(gpuType)] = limit
	}
	return gpuMap, nil
}
