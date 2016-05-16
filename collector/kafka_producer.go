package collector

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"github.com/Shopify/sarama"
	"log"
	"net"
	"os"
	"strings"
	"time"
)

var (
	brokersFlag = StringEnvFlag("kafka-brokers", "",
		"The Kafka brokers to connect to, as a comma separated list.")
	frameworkFlag = StringEnvFlag("kafka-framework", "kafka",
		"The Kafka framework to query for brokers. (overrides '-kafka-brokers')")
	flushPeriodFlag = IntEnvFlag("kafka-flush-ms", 5000,
		"Number of milliseconds to wait between output flushes")
	snappyCompressionFlag = BoolEnvFlag("kafka-compress-snappy", true,
		"Whether to enable Snappy compression on outgoing Kafka data")
	requireAllAcksFlag = BoolEnvFlag("kafka-require-all-acks", false,
		"Whether to require all replicas to commit outgoing data (true) "+
			" or to require only one replica commit (false)")
	verboseFlag = BoolEnvFlag("kafka-verbose", false,
		"Enable extra logging in the underlying Kafka client.")
)

type KafkaMessage struct {
	Topic string
	Data  []byte
}

// Creates and runs a Kafka Producer which sends records passed to the provided channel.
// This function should be run as a gofunc.
func RunKafkaProducer(messages <-chan KafkaMessage, stats chan<- StatsEvent) {
	if *verboseFlag {
		sarama.Logger = log.New(os.Stdout, "[sarama] ", log.LstdFlags)
	}

	for {
		producer, err := kafkaProducer(stats)
		if err != nil {
			stats <- StatsEvent{KafkaConnectionFailed, ""}
			log.Println("Failed to open Kafka producer:", err)
			continue
		}
		stats <- StatsEvent{KafkaSessionOpened, ""}
		defer func() {
			stats <- StatsEvent{KafkaSessionClosed, ""}
			if err := producer.Close(); err != nil {
				log.Println("Failed to shut down producer cleanly:", err)
			}
		}()
		for {
			message := <-messages
			producer.Input() <- &sarama.ProducerMessage{
				Topic: message.Topic,
				Value: sarama.ByteEncoder(message.Data),
			}
			stats <- StatsEvent{KafkaMessageSent, message.Topic}
		}
	}
}

// ---

func kafkaProducer(stats chan<- StatsEvent) (kafkaProducer sarama.AsyncProducer, err error) {
	brokers := make([]string, 0)
	if *frameworkFlag != "" {
		foundBrokers, err := lookupBrokers(*frameworkFlag)
		if err != nil {
			stats <- StatsEvent{KafkaLookupFailed, *frameworkFlag}
			return nil, errors.New(fmt.Sprintf(
				"Broker lookup against framework %s failed: %s", *frameworkFlag, err))
		}
		brokers = append(brokers, foundBrokers...)
	} else if *brokersFlag != "" {
		brokers = strings.Split(*brokersFlag, ",")
		if len(brokers) == 0 {
			log.Fatal("-brokers must be non-empty.")
		}
	} else {
		flag.Usage()
		log.Fatal("Either -framework or -brokers must be specified, or -kafka must be false.")
	}
	log.Println("Kafka brokers:", strings.Join(brokers, ", "))

	kafkaProducer, err = newAsyncProducer(brokers)
	if err != nil {
		return nil, errors.New(fmt.Sprintf(
			"Producer creation against brokers %+v failed: %s", brokers, err))
	}
	return kafkaProducer, nil
}

func newAsyncProducer(brokerList []string) (producer sarama.AsyncProducer, err error) {
	// For the access log, we are looking for AP semantics, with high throughput.
	// By creating batches of compressed messages, we reduce network I/O at a cost of more latency.
	config := sarama.NewConfig()
	if *requireAllAcksFlag {
		config.Producer.RequiredAcks = sarama.WaitForAll
	} else {
		config.Producer.RequiredAcks = sarama.WaitForLocal
	}
	if *snappyCompressionFlag {
		config.Producer.Compression = sarama.CompressionSnappy
	} else {
		config.Producer.Compression = sarama.CompressionNone
	}
	config.Producer.Flush.Frequency = time.Duration(*flushPeriodFlag) * time.Millisecond

	producer, err = sarama.NewAsyncProducer(brokerList, config)
	if err != nil {
		return nil, err
	}
	go func() {
		for err := range producer.Errors() {
			log.Println("Failed to write metrics to Kafka:", err)
		}
	}()
	return producer, nil
}

// ---

// Returns a list of broker endpoints, each of the form "host:port"
func lookupBrokers(framework string) (brokers []string, err error) {
	schedulerEndpoint, err := connectionEndpoint(framework)
	if err != nil {
		return nil, err
	}
	body, err := HttpGet(schedulerEndpoint)
	if err != nil {
		return nil, err
	}
	return extractBrokers(body)
}

func connectionEndpoint(framework string) (endpoint string, err error) {
	// SRV lookup to get scheduler's port number:
	// _framework._tcp.marathon.mesos.
	_, addrs, err := net.LookupSRV(framework, "tcp", "marathon.mesos")
	if err != nil {
		return "", err
	}
	if len(addrs) == 0 {
		return "", errors.New(fmt.Sprintf("Framework '%s' not found", framework))
	}
	url := fmt.Sprintf("http://%s.mesos:%d/v1/connection", framework, addrs[0].Port)
	log.Println("Fetching broker list from Kafka Framework at:", url)
	return url, nil
}

func extractBrokers(body []byte) (brokers []string, err error) {
	var jsonData map[string]interface{}
	if err = json.Unmarshal(body, &jsonData); err != nil {
		return nil, err
	}
	// expect "dns" entry containing a list of strings
	jsonBrokers := jsonData["dns"].([]interface{})
	brokers = make([]string, len(jsonBrokers))
	for i, jsonDnsEntry := range jsonBrokers {
		brokers[i] = jsonDnsEntry.(string)
	}
	return brokers, nil
}