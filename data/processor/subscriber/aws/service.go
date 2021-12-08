package aws

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/aws/aws-sdk-go/service/sqs"
	"github.com/viant/afs"
	"github.com/viant/cloudless/data/processor"
	"github.com/viant/cloudless/data/processor/adapter/aws"
	"github.com/viant/cloudless/data/processor/stat"
	"github.com/viant/gmetric"
	"log"
	"os"
	"reflect"
	"time"
)

//Service represents sqs service
type Service struct {
	config    *Config
	sqsClient *sqs.SQS
	queueURL  *string
	processor *processor.Service
	fs        afs.Service
	stats     *gmetric.Operation
}

//Consume starts consumer
func (s *Service) Consume(ctx context.Context) error {
	for {
		err := s.consume(ctx)
		if err != nil {
			log.Printf("failed to consume: %v\n", err)
		}
	}
}

func (s *Service) consume(ctx context.Context) error {
	var URL string
	defer func() {
		r := recover()
		if r != nil {
			fmt.Printf("recover from panic: URL:%v, error: %v", URL, r)
		}
	}()

	//fs := afs.New()
	maxNumberOfMessages := int64(s.config.BatchSize)
	waitTimeSeconds := int64(s.config.WaitTimeSeconds)
	visibilityTimeout := int64(s.config.VisibilityTimeout)
	msgs, err := s.sqsClient.ReceiveMessage(&sqs.ReceiveMessageInput{
		QueueUrl:            s.queueURL,
		MaxNumberOfMessages: &maxNumberOfMessages,
		WaitTimeSeconds:     &waitTimeSeconds,
		VisibilityTimeout:   &visibilityTimeout,
	})
	if err != nil {
		return err
	}
	for _, m := range msgs.Messages {
		s.handleMessage(ctx, m, URL, s.fs)
	}
	return nil
}

func (s *Service) deleteMessage(msg *sqs.Message) error {
	_, err := s.sqsClient.DeleteMessage(&sqs.DeleteMessageInput{
		QueueUrl:      s.queueURL,
		ReceiptHandle: msg.ReceiptHandle,
	})
	return err
}

func (s *Service) handleMessage(ctx context.Context, msg *sqs.Message, URL string, fs afs.Service) {
	recentCounter, onDone, stats := stat.SubscriberBegin(s.stats)
	defer stat.SubscriberEnd(s.stats, recentCounter, onDone, stats)

	s3Event := &aws.S3Event{}
	if err := json.Unmarshal([]byte(*msg.Body), s3Event); err != nil {
		log.Printf("failed to unmarshal GSEvent: %s, due to %v\n", *msg.Body, err)
		stats.Append(err)
		return
	}
	if len(s3Event.Records) == 0 {
		err := s.deleteMessage(msg)
		if err != nil {
			stats.Append(stat.NegativeAcknowledged)
		} else {
			stats.Append(stat.Acknowledged)
		}
		fmt.Printf("invalid event: %s\n", *msg.Body)
		return
	}
	if os.Getenv("DEBUG_MSG") != "" {
		fmt.Printf("%s\n", *msg.Body)
	}
	reqContext := context.Background()
	request, err := s3Event.NewRequest(reqContext, s.fs, &s.config.Config)
	if err != nil {
		//source file has been removed
		if exists, _ := fs.Exists(ctx, URL); !exists {
			if err = s.deleteMessage(msg);err != nil {
				stats.Append(err)
				stats.Append(stat.NegativeAcknowledged)
			} else {
				stats.Append(stat.Acknowledged)
			}
			return
		}
		stats.Append(err)
		stats.Append(stat.NegativeAcknowledged)
		log.Printf("failed to create process request from s3Event: %s, due to %v\n", *msg.Body, err)
		return
	}
	reporter := s.processor.Do(reqContext, request)
	err = s.deleteMessage(msg)
	if err != nil {
		stats.Append(err)
		stats.Append(stat.NegativeAcknowledged)
	} else {
		stats.Append(stat.Acknowledged)
	}
	output, err := json.Marshal(reporter)
	if err != nil {
		stats.Append(err)
		fmt.Printf("failed marshal reported %v\n", reporter)
	}
	fmt.Printf("%s\n", output)
}

//New creates a new sqsService
func New(config *Config, client *sqs.SQS, processor *processor.Service, fs afs.Service) (*Service, error) {
	err := config.Init(context.Background(), fs)
	if err != nil {
		return nil, err
	}
	err = config.Validate()
	if err != nil {
		return nil, err
	}
	result, err := client.GetQueueUrl(&sqs.GetQueueUrlInput{
		QueueName: &config.QueueName,
	})
	if err != nil {
		return nil, err
	}
	srv :=  &Service{
		config:    config,
		sqsClient: client,
		queueURL:  result.QueueUrl,
		processor: processor,
		fs:        fs,
	}
	if srv.config.MetricPort > 0 {
		srv.processor.StartMetricsEndpoint()
	}
	location := reflect.TypeOf(srv).PkgPath()
	srv.stats = srv.processor.Metrics.MultiOperationCounter(location, stat.SubscriberMetricName, "subscriber performance",time.Microsecond, time.Microsecond, 3 , stat.NewSubscriber())
	return srv, nil
}