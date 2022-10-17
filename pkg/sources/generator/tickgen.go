// Package generator contains an implementation of a in memory generator that generates
// payloads in json format.
package generator

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"github.com/numaproj/numaflow/pkg/udf/applier"
	"strconv"
	"sync/atomic"
	"time"

	"go.uber.org/zap"

	dfv1 "github.com/numaproj/numaflow/pkg/apis/numaflow/v1alpha1"
	"github.com/numaproj/numaflow/pkg/isb"
	"github.com/numaproj/numaflow/pkg/isb/forward"
	metricspkg "github.com/numaproj/numaflow/pkg/metrics"
	"github.com/numaproj/numaflow/pkg/shared/logging"
	"github.com/numaproj/numaflow/pkg/watermark/fetch"
	"github.com/numaproj/numaflow/pkg/watermark/processor"
	"github.com/numaproj/numaflow/pkg/watermark/publish"
	"github.com/numaproj/numaflow/pkg/watermark/store"
)

var log = logging.NewLogger()
var timeAttr = "Createdts"

// payload generated by the generator function
// look at newreadmessage function
type payload struct {
	Data      []byte
	Createdts int64
}

// record is payload with offset
// internal construct of this package
type record struct {
	data   []byte
	offset int64
}

var recordGenerator = func(size int32) []byte {
	nano := time.Now().UnixNano()
	b := make([]byte, size)
	binary.LittleEndian.PutUint64(b, uint64(nano))

	r := payload{Data: b, Createdts: nano}
	data, err := json.Marshal(r)
	if err != nil {
		log.Errorf("error marshalling the record [%v]", r)
	}
	return data
}

type memgen struct {
	// srcchan provides a go channel that supplies generated data
	srcchan chan record
	// rpu - records per time unit
	rpu int
	// msgSize is the size of each generated message
	msgSize int32
	// timeunit - ticker will fire once per timeunit and generates
	// a number of records equal to the number passed to rpu.
	timeunit time.Duration
	// genfn function that generates a payload as a byte array
	genfn func(int32) []byte
	// name is the name of the source node
	name string
	// pipelineName is the name of the pipeline
	pipelineName string
	// cancel function .
	// once terminated the source will not generate any more records.
	cancel context.CancelFunc
	// forwarder to read from the source and write to the interstep buffer.
	forwarder *forward.InterStepDataForward
	// lifecycleCtx context is used to control the lifecycle of this instance.
	lifecycleCtx context.Context
	// read timeout for the reader
	readTimeout time.Duration

	// vertex instance
	vertexInstance *dfv1.VertexInstance
	// source watermark publisher
	sourcePublishWM publish.Publisher

	logger *zap.SugaredLogger
}

type Option func(*memgen) error

// WithLogger is used to return logger information
func WithLogger(l *zap.SugaredLogger) Option {
	return func(o *memgen) error {
		o.logger = l
		return nil
	}
}

func WithReadTimeOut(timeout time.Duration) Option {
	return func(o *memgen) error {
		o.readTimeout = timeout
		return nil
	}
}

// NewMemGen fuction creates an instance of generator.
// ctx  - context passed by the cmd/start.go a new context with cancel
//
//	is created for use by this vertex.
//
// name - name of this vertex
// rpu  - no of records to generate per time unit. by default the channel buffer size is set to 5*rpu
// msgSize - size of each generated message
// timeunit - unit of time per tick. could be any golang time.Duration.
// writers - destinations to write to
func NewMemGen(vertexInstance *dfv1.VertexInstance,
	rpu int,
	msgSize int32,
	timeunit time.Duration,
	writers []isb.BufferWriter,
	fetchWM fetch.Fetcher, publishWM map[string]publish.Publisher, publishWMStores store.WatermarkStorer, // watermarks
	opts ...Option) (*memgen, error) {
	gensrc := &memgen{
		rpu:            rpu,
		msgSize:        msgSize,
		timeunit:       timeunit,
		name:           vertexInstance.Vertex.Spec.Name,
		pipelineName:   vertexInstance.Vertex.Spec.PipelineName,
		genfn:          recordGenerator,
		vertexInstance: vertexInstance,
		srcchan:        make(chan record, rpu*5),
		readTimeout:    3 * time.Second, // default timeout
	}

	for _, o := range opts {
		if err := o(gensrc); err != nil {
			return nil, err
		}
	}
	if gensrc.logger == nil {
		gensrc.logger = logging.NewLogger()
	}

	// this context is to be used internally for controlling the lifecycle of generator
	cctx, cancel := context.WithCancel(context.Background())

	gensrc.lifecycleCtx = cctx
	gensrc.cancel = cancel

	destinations := make(map[string]isb.BufferWriter, len(writers))
	for _, w := range writers {
		destinations[w.GetName()] = w
	}

	forwardOpts := []forward.Option{forward.FromSourceVertex(), forward.WithLogger(gensrc.logger)}
	if x := vertexInstance.Vertex.Spec.Limits; x != nil {
		if x.ReadBatchSize != nil {
			forwardOpts = append(forwardOpts, forward.WithReadBatchSize(int64(*x.ReadBatchSize)))
		}
	}

	// attach a source publisher so the source can assign the watermarks.
	gensrc.sourcePublishWM = gensrc.buildSourceWatermarkPublisher(publishWMStores)

	// we pass in the context to forwarder as well so that it can shut down when we cancel the context
	forwarder, err := forward.NewInterStepDataForward(vertexInstance.Vertex, gensrc, destinations, forward.All, applier.Terminal, fetchWM, publishWM, forwardOpts...)
	if err != nil {
		return nil, err
	}
	gensrc.forwarder = forwarder

	return gensrc, nil
}

func (mg *memgen) GetName() string {
	return mg.name
}

func (mg *memgen) IsEmpty() bool {
	return len(mg.srcchan) == 0
}

// Read reads a chunk of messages and returns at the first occurrence of an error. Error does not indicate that the
// array of result is empty, the callee should process all the elements in the array even if the error is set. Read
// will not mark the message in the buffer as "READ" if the read for that index is erring.
// There is a chance that we have read the message and the container got forcefully terminated before processing. To provide
// at-least-once semantics for reading, during restart we will have to reprocess all unacknowledged messages.
//
// the context passed to read should be different from the lifecycle context that is used by this vertex.
func (mg *memgen) Read(ctx context.Context, count int64) ([]*isb.ReadMessage, error) {
	msgs := make([]*isb.ReadMessage, 0, count)
	// timeout should not be re-triggered for every run of the for loop. it is for the entire Read() call.
	timeout := time.After(mg.readTimeout)
loop:
	for i := int64(0); i < count; i++ {
		// since the Read call is blocking, and runs in an infinite loop
		// we implement Read With Wait semantics
		select {
		case r := <-mg.srcchan:
			tickgenSourceReadCount.With(map[string]string{metricspkg.LabelVertex: mg.name, metricspkg.LabelPipeline: mg.pipelineName}).Inc()
			msgs = append(msgs, newreadmessage(r.data, r.offset))
		case <-timeout:
			mg.logger.Debugw("Timed out waiting for messages to read.", zap.Duration("waited", mg.readTimeout))
			break loop
		}
	}
	if len(msgs) > 0 {
		// publish the last message's offset with watermark, this is an optimization to avoid too many insert calls
		// into the offset timeline store.
		// Please note that we are inserting the watermark before the data has been persisted into ISB by the forwarder.
		o := msgs[len(msgs)-1].ReadOffset
		nanos, _ := o.Sequence()
		// remove the nanosecond precision
		mg.sourcePublishWM.PublishWatermark(processor.Watermark(time.Unix(0, nanos)), o)
	}
	return msgs, nil
}

// Ack acknowledges an array of offset.
func (mg *memgen) Ack(_ context.Context, offsets []isb.Offset) []error {
	return make([]error, len(offsets))
}

func (br *memgen) Close() error {
	return nil
}

func (mg *memgen) Stop() {
	mg.cancel()
	mg.forwarder.Stop()
}

func (mg *memgen) ForceStop() {
	mg.Stop()
	mg.forwarder.ForceStop()

}

// Start starts reading from the source
// context is used to control the lifecycle of this component.
// this context will be used to shutdown the vertex once a os.signal is received.
func (mg *memgen) Start() <-chan struct{} {
	mg.generator(mg.lifecycleCtx, mg.rpu, mg.timeunit)
	return mg.forwarder.Start()
}

// generator fires once per time unit and generates records and writes them to the channel
func (mg *memgen) generator(ctx context.Context, rate int, timeunit time.Duration) {
	go func() {
		var rcount int32 = 0
		// we are capping the limit at 10000 msgs / second
		var limit = int32(10000 / rate)
		ticker := time.NewTicker(timeunit)
		defer ticker.Stop()
		for {
			select {
			// we dont need to wait for ticker to fire to return
			// when context closes
			case <-ctx.Done():
				log.Info("Context.Done is called. exiting generator loop.")
				return
			case <-ticker.C:
				tickgenSourceCount.With(map[string]string{metricspkg.LabelVertex: mg.name, metricspkg.LabelPipeline: mg.pipelineName})
				// swapped implies that the rcount is at limit
				if !atomic.CompareAndSwapInt32(&rcount, limit-1, limit) {
					go func() {
						atomic.AddInt32(&rcount, 1)
						defer atomic.AddInt32(&rcount, -1)
						for i := 0; i < rate; i++ {
							payload := mg.genfn(mg.msgSize)
							r := record{data: payload, offset: time.Now().UTC().UnixNano()}
							select {
							case <-ctx.Done():
								log.Info("Context.Done is called. returning from the inner function")
								return
							case mg.srcchan <- r:

							}
						}
					}()
				}
			}
		}
	}()
}

func newreadmessage(payload []byte, offset int64) *isb.ReadMessage {
	msg := isb.Message{
		Header: isb.Header{
			// TODO: insert the right time based on the generator
			PaneInfo: isb.PaneInfo{EventTime: timefromNanos(parseTime(payload))},
			ID:       strconv.FormatInt(offset, 10),
		},
		Body: isb.Body{Payload: payload},
	}

	return &isb.ReadMessage{
		ReadOffset: isb.SimpleOffset(func() string { return strconv.FormatInt(offset, 10) }),
		Message:    msg,
	}
}

func timefromNanos(etime int64) time.Time {
	// unparseable json or invalid time format will be substituted with current time.
	if etime > 0 {
		return time.Unix(0, etime)
	}
	return time.Now()
}

func parseTime(payload []byte) int64 {

	var anyJson map[string]interface{}
	unmarshalErr := json.Unmarshal(payload, &anyJson)

	if unmarshalErr != nil {
		log.Debug("Payload [{}] is not valid json. could not extract time, returning 0", payload)
		return 0
	}

	// for now lets pretend that the time unit is nanos and that the time attribute is known
	eventTime := anyJson[timeAttr]
	if i, ok := eventTime.(float64); ok {
		return int64(i)
	} else {
		return 0
	}

}
