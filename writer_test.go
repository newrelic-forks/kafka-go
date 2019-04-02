package kafka

import (
	"context"
	"errors"
	"io"
	"math"
	"strings"
	"testing"
	"time"
)

func TestWriter(t *testing.T) {
	t.Parallel()

	tests := []struct {
		scenario string
		function func(*testing.T)
	}{
		{
			scenario: "closing a writer right after creating it returns promptly with no error",
			function: testWriterClose,
		},

		{
			scenario: "writing 1 message through a writer using round-robin balancing produces 1 message to the first partition",
			function: testWriterRoundRobin1,
		},

		{
			scenario: "running out of max attempts should return an error",
			function: testWriterMaxAttemptsErr,
		},
		{
			scenario: "writing a message larger then the max bytes should return an error",
			function: testWriterMaxBytes,
		},
		{
			scenario: "writing a batch of message based on batch byte size",
			function: testWriterBatchBytes,
		},
		{
			scenario: "writing a batch of messages",
			function: testWriterBatchSize,
		},
		{
			scenario: "writing messsages with a small batch byte size",
			function: testWriterSmallBatchBytes,
		},
	}

	for _, test := range tests {
		testFunc := test.function
		t.Run(test.scenario, func(t *testing.T) {
			t.Parallel()
			testFunc(t)
		})
	}
}

func TestIntWriter(t *testing.T) {
	t.Parallel()
	tests := []struct {
		scenario string
		function func(*testing.T)
	}{
		{
			scenario: "writing messages that will error to test retries",
			function: testIntWriterRetryErr,
		},
	}
	for _, test := range tests {
		testFunc := test.function
		t.Run(test.scenario, func(t *testing.T) {
			t.Parallel()
			testFunc(t)
		})
	}
}

func newTestWriter(config WriterConfig) *Writer {
	if len(config.Brokers) == 0 {
		config.Brokers = []string{"localhost:9092"}
	}
	return NewWriter(config)
}

func testWriterClose(t *testing.T) {
	const topic = "test-writer-0"

	createTopic(t, topic, 1)
	w := newTestWriter(WriterConfig{
		Topic: topic,
	})

	if err := w.Close(); err != nil {
		t.Error(err)
	}
}

func testWriterRoundRobin1(t *testing.T) {
	const topic = "test-writer-1"

	createTopic(t, topic, 1)
	offset, err := readOffset(topic, 0)
	if err != nil {
		t.Fatal(err)
	}

	w := newTestWriter(WriterConfig{
		Topic:    topic,
		Balancer: &RoundRobin{},
	})
	defer w.Close()

	if err := w.WriteMessages(context.Background(), Message{
		Value: []byte("Hello World!"),
	}); err != nil {
		t.Error(err)
		return
	}

	msgs, err := readPartition(topic, 0, offset)

	if err != nil {
		t.Error("error reading partition", err)
		return
	}

	if len(msgs) != 1 {
		t.Error("bad messages in partition", msgs)
		return
	}

	for _, m := range msgs {
		if string(m.Value) != "Hello World!" {
			t.Error("bad messages in partition", msgs)
			break
		}
	}
}

type fakeWriter struct{}

func (f *fakeWriter) messages() chan<- writerMessage {
	ch := make(chan writerMessage, 1)

	go func() {
		for {
			msg := <-ch
			msg.res <- &writerError{
				err: errors.New("bad attempt"),
			}
		}
	}()

	return ch
}

func (f *fakeWriter) close() {

}

func testWriterMaxAttemptsErr(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	topic := makeTopic()
	createTopic(t, topic, 1)
	w := newTestWriter(WriterConfig{
		Topic:       topic,
		MaxAttempts: 1,
		Balancer:    &RoundRobin{},
		newPartitionWriter: func(p int, config WriterConfig, stats *writerStats) partitionWriter {
			return &fakeWriter{}
		},
	})
	defer w.Close()
	if err := w.WriteMessages(ctx, Message{
		Value: []byte("Hello World!"),
	}); err == nil {
		t.Error("expected error")
		return
	} else if err != nil {
		if !strings.Contains(err.Error(), "bad attempt") {
			t.Errorf("unexpected error: %s", err)
			return
		}
	}
}

func testWriterMaxBytes(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	topic := makeTopic()
	createTopic(t, topic, 1)
	w := newTestWriter(WriterConfig{
		Topic:      topic,
		BatchSize:  1,
		BatchBytes: 25,
	})
	defer w.Close()

	if err := w.WriteMessages(ctx, Message{
		Value: []byte("Hi"),
	}); err != nil {
		t.Error(err)
		return
	}
	if err := w.WriteMessages(ctx, Message{
		Value: []byte("Hello World!"),
	}); err != nil {
		t.Error("got error when not expecting one: ", err)
		return
	}
	if w.Stats().Errors < 1 {
		t.Error("exepecting error count to be at least 1 due to max message bytes")
	}
	return
}

func readOffset(topic string, partition int) (offset int64, err error) {
	var conn *Conn

	if conn, err = DialLeader(context.Background(), "tcp", "localhost:9092", topic, partition); err != nil {
		return
	}
	defer conn.Close()

	offset, err = conn.ReadLastOffset()
	return
}

func readPartition(topic string, partition int, offset int64) (msgs []Message, err error) {
	var conn *Conn

	if conn, err = DialLeader(context.Background(), "tcp", "localhost:9092", topic, partition); err != nil {
		return
	}
	defer conn.Close()

	conn.Seek(offset, SeekAbsolute)
	conn.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
	batch := conn.ReadBatch(0, 1000000000)
	defer batch.Close()

	for {
		var msg Message

		if msg, err = batch.ReadMessage(); err != nil {
			if err == io.EOF {
				err = nil
			}
			return
		}

		msgs = append(msgs, msg)
	}
}

func testWriterBatchBytes(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	const topic = "test-writer-1-bytes"

	createTopic(t, topic, 1)
	offset, err := readOffset(topic, 0)
	if err != nil {
		t.Fatal(err)
	}

	w := newTestWriter(WriterConfig{
		Topic:        topic,
		BatchBytes:   48,
		BatchTimeout: math.MaxInt32 * time.Second,
		Balancer:     &RoundRobin{},
	})
	defer w.Close()

	if err := w.WriteMessages(ctx, []Message{
		Message{Value: []byte("Hi")}, // 24 Bytes
		Message{Value: []byte("By")}, // 24 Bytes
	}...); err != nil {
		t.Error(err)
		return
	}

	if w.Stats().Writes > 1 {
		t.Error("didn't batch messages")
		return
	}
	msgs, err := readPartition(topic, 0, offset)

	if err != nil {
		t.Error("error reading partition", err)
		return
	}

	if len(msgs) != 2 {
		t.Error("bad messages in partition", msgs)
		return
	}

	for _, m := range msgs {
		if string(m.Value) == "Hi" || string(m.Value) == "By" {
			continue
		}
		t.Error("bad messages in partition", msgs)
	}
}

func testWriterBatchSize(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	topic := makeTopic()
	createTopic(t, topic, 1)
	offset, err := readOffset(topic, 0)
	if err != nil {
		t.Fatal(err)
	}

	w := newTestWriter(WriterConfig{
		Topic:        topic,
		BatchSize:    2,
		BatchTimeout: math.MaxInt32 * time.Second,
		Balancer:     &RoundRobin{},
	})
	defer w.Close()

	if err := w.WriteMessages(ctx, []Message{
		Message{Value: []byte("Hi")}, // 24 Bytes
		Message{Value: []byte("By")}, // 24 Bytes
	}...); err != nil {
		t.Error(err)
		return
	}

	if w.Stats().Writes > 1 {
		t.Error("didn't batch messages")
		return
	}
	msgs, err := readPartition(topic, 0, offset)

	if err != nil {
		t.Error("error reading partition", err)
		return
	}

	if len(msgs) != 2 {
		t.Error("bad messages in partition", msgs)
		return
	}

	for _, m := range msgs {
		if string(m.Value) == "Hi" || string(m.Value) == "By" {
			continue
		}
		t.Error("bad messages in partition", msgs)
	}
}

func testWriterSmallBatchBytes(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	topic := makeTopic()
	createTopic(t, topic, 1)
	offset, err := readOffset(topic, 0)
	if err != nil {
		t.Fatal(err)
	}

	w := newTestWriter(WriterConfig{
		Topic:             topic,
		BatchBytes:        25,
		BatchTimeout:      500 * time.Millisecond,
		Balancer:          &RoundRobin{},
		RebalanceInterval: 1 * time.Second,
	})
	defer w.Close()

	if err := w.WriteMessages(ctx, []Message{
		Message{Value: []byte("Hi")}, // 24 Bytes
		Message{Value: []byte("By")}, // 24 Bytes
	}...); err != nil {
		t.Error(err)
		return
	}

	if w.Stats().Writes != 2 {
		t.Error("didn't batch messages")
		return
	}
	msgs, err := readPartition(topic, 0, offset)

	if err != nil {
		t.Error("error reading partition", err)
		return
	}

	if len(msgs) != 2 {
		t.Error("bad messages in partition", msgs)
		return
	}

	for _, m := range msgs {
		if string(m.Value) == "Hi" || string(m.Value) == "By" {
			continue
		}
		t.Error("bad messages in partition", msgs)
	}
}

func testIntWriterRetryErr(t *testing.T) {
	//ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	//defer cancel()

	topic := makeTopic()
	createTopic(t, topic, 1)
	offset, err := readOffset(topic, 0)
	if err != nil {
		t.Fatal(err)
	}
	stats := &writerStats{}
	w := newWriter(1, WriterConfig{
		Topic: topic,
		//Give it a bad broker so we can test network failures
		Brokers:      []string{"localhost:9099"},
		BatchSize:    10,
		MaxAttempts:  1,
		Retries:      5,
		Dialer:       DefaultDialer,
		WriteTimeout: 10 * time.Second,
	}, stats)
	w.partition = 0
	defer w.close()
	errc := make([](chan<- error), 0, 200)

	failedBatch := []Message{
		Message{Value: []byte("NetworkError")},
		Message{Value: []byte("CantFindMe")},
	}

	_, err = w.write(nil, failedBatch, errc)
	if err == nil {
		t.Error("expected error, got nothing")
	}
	ssnap := stats.retries.snapshot()
	if ssnap.Max != 5 {
		t.Error("Expect retries to be equal to retry count")
	}

	//First We Tested Bad, now we test a good connection.
	// We'll use that good connection at the end to create
	// a new nother bad test.
	w.brokers = []string{"localhost:9092"}
	gcnn, err := w.write(nil, []Message{
		Message{Value: []byte("FindMe")},
	}, errc)
	if err != nil {
		t.Error("expected no error, got error: ", err)
	}
	msgs, err := readPartition(topic, 0, offset)
	if err != nil {
		t.Error("expected no error, got error: ", err)
	}
	if len(msgs) != 1 {
		t.Errorf("bad messages in partition %+v ", msgs)
		return
	}
	for _, m := range msgs {
		if string(m.Value) == "FindMe" {
			continue
		}
		t.Error("didn't read any messages")
	}

	w.writeTimeout = 0 * time.Second
	_, err = w.write(gcnn, []Message{
		Message{Value: []byte("BadBroker")},
	}, errc)
	if err == nil {
		t.Error("expected error, got nothing")
	}
	ssnap = stats.retries.snapshot()
	if ssnap.Max != 5 {
		t.Error("Expect retries to be equal to retry count")
	}
}
