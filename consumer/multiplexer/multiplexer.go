package multiplexer

import (
	"reflect"
	"sort"
	"sync"

	"github.com/mailgun/kafka-pixy/actor"
	"github.com/mailgun/kafka-pixy/consumer"
	"github.com/mailgun/kafka-pixy/none"
	"github.com/mailgun/log"
)

// T fetches messages from inputs and multiplexes them to the output, giving
// preferences to inputs with higher lag. Multiplexes assumes ownership over
// inputs in the sense that it decides when an new input instance needs to
// started, or the old one stopped.
type T struct {
	actorID   *actor.ID
	spawnInF  SpawnInF
	inputs    map[int32]*input
	output    Out
	isRunning bool
	stopCh    chan none.T
	wg        sync.WaitGroup
}

// In defines an interface of a multiplexer input.
type In interface {
	// Messages returns channel that multiplexer receives messages from.
	Messages() <-chan *consumer.Message

	// Acks returns a channel that multiplexer sends a message that was pulled
	// from the `Messages()` channel of this input after the message has been
	// send to the output.
	Acks() chan<- *consumer.Message

	// Stop signals the input to stop and blocks waiting for its goroutines to
	// complete.
	Stop()
}

// Out defines an interface of multiplexer output.
type Out interface {
	// Messages returns channel that multiplexer sends messages to.
	Messages() chan<- *consumer.Message
}

// SpawnInF is a function type that is used by multiplexer to spawn inputs for
// assigned partitions during rewiring.
type SpawnInF func(partition int32) In

// New creates a new multiplexer instance.
func New(namespace *actor.ID, spawnInF SpawnInF) *T {
	return &T{
		actorID:  namespace.NewChild("mux"),
		inputs:   make(map[int32]*input),
		spawnInF: spawnInF,
		stopCh:   make(chan none.T),
	}
}

// input represents a multiplexer input along with a message to be fetched from
// that input next.
type input struct {
	In
	partition int32
	nextMsg   *consumer.Message
}

// IsRunning returns `true` if multiplexer is running pumping events from the
// inputs to the output.
func (m *T) IsRunning() bool {
	return m.isRunning
}

// WireUp ensures that assigned inputs are spawned and multiplexed to the
// specified output. It stops inputs for partitions that are no longer
// assigned, spawns inputs for newly assigned partitions, and restarts the
// multiplexer, if either output or any of inputs has changed.
//
// The multiplexer may be stopped if either output or all inputs are removed.
//
// WARNING: do not ever pass (*T)(nil) in output, that will cause panic.
func (m *T) WireUp(output Out, assigned []int32) {
	var wg sync.WaitGroup

	if m.output != output {
		m.stopIfRunning()
		m.output = output
	}
	// If output is not provided, then stop all inputs and return.
	if output == nil {
		for p, in := range m.inputs {
			wg.Add(1)
			go func(in *input) {
				defer wg.Done()
				in.Stop()
			}(in)
			delete(m.inputs, p)
		}
		wg.Wait()
		return
	}
	// Stop inputs that are not assigned anymore.
	for p, in := range m.inputs {
		if !hasPartition(p, assigned) {
			m.stopIfRunning()
			wg.Add(1)
			go func(in *input) {
				defer wg.Done()
				in.Stop()
			}(in)
			delete(m.inputs, p)
		}
	}
	wg.Wait()
	// Spawn newly assigned inputs, but stop multiplexer before spawning the
	// first input.
	for _, p := range assigned {
		if _, ok := m.inputs[p]; !ok {
			m.stopIfRunning()
			m.inputs[p] = &input{In: m.spawnInF(p), partition: p}
		}
	}
	if !m.IsRunning() && len(m.inputs) > 0 {
		m.start()
	}
}

// Stop synchronously stops the multiplexer.
func (m *T) Stop() {
	m.WireUp(nil, nil)
}

func (m *T) start() {
	actor.Spawn(m.actorID, &m.wg, m.run)
	m.isRunning = true
}

func (m *T) stopIfRunning() {
	if m.isRunning {
		m.stopCh <- none.V
		m.wg.Wait()
		m.isRunning = false
	}
}

func (m *T) run() {
reset:
	inputCount := len(m.inputs)
	if inputCount == 0 {
		return
	}
	sortedIns := makeSortedIns(m.inputs)
	// Prepare a list of reflective select cases. It is used when none of the
	// inputs has fetched messages and we need to wait on all of them. Yes,
	// reflection is slow, but it is only used when there is nothing to
	// consume anyway.
	selectCases := make([]reflect.SelectCase, inputCount+1)
	for i, in := range sortedIns {
		selectCases[i] = reflect.SelectCase{Dir: reflect.SelectRecv, Chan: reflect.ValueOf(in.Messages())}
	}
	selectCases[inputCount] = reflect.SelectCase{Dir: reflect.SelectRecv, Chan: reflect.ValueOf(m.stopCh)}

	inputIdx := -1
	for {
		// Collect next messages from inputs that have them available.
		isAtLeastOneAvailable := false
		for _, in := range sortedIns {
			if in.nextMsg != nil {
				isAtLeastOneAvailable = true
				continue
			}
			select {
			case msg, ok := <-in.Messages():
				// If a channel of an input is closed, then the input should be
				// removed from the list of multiplexed inputs.
				if !ok {
					log.Infof("<%s> input channel closed: partition=%d", in.partition)
					delete(m.inputs, in.partition)
					goto reset
				}
				in.nextMsg = msg
				isAtLeastOneAvailable = true
			default:
			}
		}
		// If none of the inputs has a message available, then wait until
		// a message is fetched on any of them or a stop signal is received.
		if !isAtLeastOneAvailable {
			idx, value, _ := reflect.Select(selectCases)
			// Check if it is a stop signal.
			if idx == inputCount {
				return
			}
			sortedIns[idx].nextMsg = value.Interface().(*consumer.Message)
		}
		// At this point there is at least one message available.
		inputIdx = selectInput(inputIdx, sortedIns)
		// Block until the output reads the next message of the selected input
		// or a stop signal is received.
		select {
		case <-m.stopCh:
			return
		case m.output.Messages() <- sortedIns[inputIdx].nextMsg:
			sortedIns[inputIdx].Acks() <- sortedIns[inputIdx].nextMsg
			sortedIns[inputIdx].nextMsg = nil
		}
	}
}

// makeSortedIns given a partition->input map returns a slice of all the inputs
// from the map sorted in ascending order of partition ids.
func makeSortedIns(inputs map[int32]*input) []*input {
	partitions := make([]int32, 0, len(inputs))
	for p := range inputs {
		partitions = append(partitions, p)
	}
	sort.Sort(Int32Slice(partitions))
	sortedIns := make([]*input, len(inputs))
	for i, p := range partitions {
		sortedIns[i] = inputs[p]
	}
	return sortedIns
}

func hasPartition(partition int32, partitions []int32) bool {
	count := len(partitions)
	if count == 0 {
		return false
	}
	return partitions[0] <= partition && partition <= partitions[count-1]
}

// selectInput picks an input that should be multiplexed next. It prefers the
// inputs with the largest lag. If there is more then one input with the same
// largest lag, then it picks the one that has index following prevSelectedIdx.
func selectInput(prevSelectedIdx int, sortedIns []*input) int {
	maxLag := int64(-1)
	selectedIdx := -1
	for i, input := range sortedIns {
		if input.nextMsg == nil {
			continue
		}
		lag := input.nextMsg.HighWaterMark - input.nextMsg.Offset
		if lag > maxLag {
			maxLag = lag
			selectedIdx = i
			continue
		}
		if lag < maxLag {
			continue
		}
		if selectedIdx > prevSelectedIdx {
			continue
		}
		if i > prevSelectedIdx {
			selectedIdx = i
		}
	}
	return selectedIdx
}

type Int32Slice []int32

func (p Int32Slice) Len() int           { return len(p) }
func (p Int32Slice) Less(i, j int) bool { return p[i] < p[j] }
func (p Int32Slice) Swap(i, j int)      { p[i], p[j] = p[j], p[i] }
