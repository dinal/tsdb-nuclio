/*
Copyright 2018 Iguazio Systems Ltd.

Licensed under the Apache License, Version 2.0 (the "License") with
an addition restriction as set forth herein. You may not use this
file except in compliance with the License. You may obtain a copy of
the License at http://www.apache.org/licenses/LICENSE-2.0.

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or
implied. See the License for the specific language governing
permissions and limitations under the License.

In addition, you may not use the software for any purposes that are
illegal under applicable law, and the grant of the foregoing license
under the Apache 2.0 license is conditioned upon your compliance with
such restriction.
*/

package appender

import (
	"fmt"
	"sync"
	"time"

	"github.com/nuclio/logger"
	"github.com/pkg/errors"
	"github.com/v3io/v3io-go/pkg/dataplane"
	"github.com/v3io/v3io-tsdb/internal/pkg/performance"
	"github.com/v3io/v3io-tsdb/pkg/config"
	"github.com/v3io/v3io-tsdb/pkg/partmgr"
	"github.com/v3io/v3io-tsdb/pkg/utils"
)

// TODO: make configurable
const maxRetriesOnWrite = 3
const channelSize = 4096
const queueStallTime = 1 * time.Millisecond

const minimalUnixTimeMs = 0          // year 1970
const maxUnixTimeMs = 13569465600000 // year 2400

// to add, rollups policy (cnt, sum, min/max, sum^2) + interval , or policy in per name label
type MetricState struct {
	sync.RWMutex
	state storeState
	Lset  utils.LabelsIfc
	key   string
	name  string
	hash  uint64
	refID uint64

	aggrs []*MetricState

	store      *chunkStore
	err        error
	retryCount uint8
	newName    bool
	isVariant  bool

	shouldGetState bool
}

// Metric store states
type storeState uint8

const (
	storeStateInit          storeState = 0
	storeStatePreGet        storeState = 1 // Need to get state
	storeStateGet           storeState = 2 // Getting old state from storage
	storeStateReady         storeState = 3 // Ready to update
	storeStateUpdate        storeState = 4 // Update/write in progress
	storeStateAboutToUpdate storeState = 5 // Like ready state but with updates pending
)

// store is ready to update samples into the DB
func (m *MetricState) isReady() bool {
	return m.state == storeStateReady
}

// Indicates whether the metric has no inflight requests and can send new ones
func (m *MetricState) canSendRequests() bool {
	return m.state == storeStateReady || m.state == storeStateAboutToUpdate
}

func (m *MetricState) getState() storeState {
	return m.state
}

func (m *MetricState) setState(state storeState) {
	m.state = state
}

func (m *MetricState) setError(err error) {
	m.err = err
}

func (m *MetricState) error() error {
	m.RLock()
	defer m.RUnlock()
	return m.err
}

type cacheKey struct {
	name string
	hash uint64
}

// store the state and metadata for all the metrics
type MetricsCache struct {
	cfg           *config.V3ioConfig
	partitionMngr *partmgr.PartitionManager
	mtx           sync.RWMutex
	container     v3io.Container
	logger        logger.Logger
	started       bool

	responseChan    chan *v3io.Response
	nameUpdateChan  chan *v3io.Response
	asyncAppendChan chan *asyncAppend
	updatesInFlight int

	metricQueue     *ElasticQueue
	updatesComplete chan int
	newUpdates      chan int

	outstandingUpdates int64
	requestsInFlight   int64

	lastMetric uint64

	// TODO: consider switching to synch.Map (https://golang.org/pkg/sync/#Map)
	cacheMetricMap map[cacheKey]*MetricState // TODO: maybe use hash as key & combine w ref
	cacheRefMap    map[uint64]*MetricState   // TODO: maybe turn to list + free list, periodically delete old matrics

	NameLabelMap map[string]bool // temp store all lable names

	lastError           error
	performanceReporter *performance.MetricReporter

	stopChan chan int
}

func NewMetricsCache(container v3io.Container, logger logger.Logger, cfg *config.V3ioConfig,
	partMngr *partmgr.PartitionManager) *MetricsCache {

	newCache := MetricsCache{container: container, logger: logger, cfg: cfg, partitionMngr: partMngr}
	newCache.cacheMetricMap = map[cacheKey]*MetricState{}
	newCache.cacheRefMap = map[uint64]*MetricState{}

	newCache.responseChan = make(chan *v3io.Response, channelSize)
	newCache.nameUpdateChan = make(chan *v3io.Response, channelSize)
	newCache.asyncAppendChan = make(chan *asyncAppend, channelSize)

	newCache.metricQueue = NewElasticQueue()
	newCache.newUpdates = make(chan int, 1000)
	newCache.stopChan = make(chan int, 3)

	newCache.NameLabelMap = map[string]bool{}
	newCache.performanceReporter = performance.ReporterInstanceFromConfig(cfg)

	return &newCache
}

type asyncAppend struct {
	metric       *MetricState
	t            int64
	v            interface{}
	resp         chan int
	isCompletion bool
}

func (mc *MetricsCache) Start() error {
	err := mc.start()
	if err != nil {
		return errors.Wrap(err, "Failed to start Appender loop")
	}

	return nil
}

// return metric struct by key
func (mc *MetricsCache) getMetric(name string, hash uint64) (*MetricState, bool) {
	mc.mtx.RLock()
	defer mc.mtx.RUnlock()

	metric, ok := mc.cacheMetricMap[cacheKey{name, hash}]
	return metric, ok
}

// create a new metric and save in the map
func (mc *MetricsCache) addMetric(hash uint64, name string, metric *MetricState) {
	mc.mtx.Lock()
	defer mc.mtx.Unlock()

	mc.lastMetric++
	metric.refID = mc.lastMetric
	mc.cacheRefMap[mc.lastMetric] = metric
	mc.cacheMetricMap[cacheKey{name, hash}] = metric
	if _, ok := mc.NameLabelMap[name]; !ok {
		metric.newName = true
		mc.NameLabelMap[name] = true
	}
}

// return metric struct by refID
func (mc *MetricsCache) getMetricByRef(ref uint64) (*MetricState, bool) {
	mc.mtx.RLock()
	defer mc.mtx.RUnlock()

	metric, ok := mc.cacheRefMap[ref]
	return metric, ok
}

// Push append to async channel
func (mc *MetricsCache) appendTV(metric *MetricState, t int64, v interface{}) {
	mc.asyncAppendChan <- &asyncAppend{metric: metric, t: t, v: v}
}

// First time add time & value to metric (by label set)
func (mc *MetricsCache) Add(lset utils.LabelsIfc, t int64, v interface{}) (uint64, error) {

	err := verifyTimeValid(t)
	if err != nil {
		return 0, err
	}

	var isValueVariantType bool
	// If the value is not of Float type assume it's variant type.
	switch v.(type) {
	case int, int64, float64, float32:
		isValueVariantType = false
	default:
		isValueVariantType = true
	}

	name, key, hash := lset.GetKey()
	err = utils.IsValidMetricName(name)
	if err != nil {
		return 0, err
	}
	metric, ok := mc.getMetric(name, hash)

	var aggrMetrics []*MetricState
	if !ok {
		for _, preAggr := range mc.partitionMngr.GetConfig().TableSchemaInfo.PreAggregates {
			subLset := lset.Filter(preAggr.Labels)
			name, key, hash := subLset.GetKey()
			aggrMetric, ok := mc.getMetric(name, hash)
			if !ok {
				aggrMetric = &MetricState{Lset: subLset, key: key, name: name, hash: hash}
				aggrMetric.store = newChunkStore(mc.logger, subLset.LabelNames(), true)
				mc.addMetric(hash, name, aggrMetric)
				aggrMetrics = append(aggrMetrics, aggrMetric)
			}
		}
		metric = &MetricState{Lset: lset, key: key, name: name, hash: hash,
			aggrs: aggrMetrics, isVariant: isValueVariantType}

		metric.store = newChunkStore(mc.logger, lset.LabelNames(), false)
		mc.addMetric(hash, name, metric)
	} else {
		aggrMetrics = metric.aggrs
	}

	err = metric.error()
	metric.setError(nil)

	if isValueVariantType != metric.isVariant {
		newValueType := "numeric"
		if isValueVariantType {
			newValueType = "string"
		}
		existingValueType := "numeric"
		if metric.isVariant {
			existingValueType = "string"
		}
		return 0, errors.Errorf("Cannot append %v type metric to %v type metric.", newValueType, existingValueType)
	}

	mc.appendTV(metric, t, v)
	for _, aggrMetric := range aggrMetrics {
		mc.appendTV(aggrMetric, t, v)
	}

	return metric.refID, err
}

// fast Add to metric (by refID)
func (mc *MetricsCache) AddFast(ref uint64, t int64, v interface{}) error {

	err := verifyTimeValid(t)
	if err != nil {
		return err
	}

	metric, ok := mc.getMetricByRef(ref)
	if !ok {
		mc.logger.ErrorWith("Ref not found", "ref", ref)
		return fmt.Errorf("ref not found")
	}

	err = metric.error()
	metric.setError(nil)

	mc.appendTV(metric, t, v)

	for _, aggrMetric := range metric.aggrs {
		mc.appendTV(aggrMetric, t, v)
	}

	return err
}

func verifyTimeValid(t int64) error {
	if t > maxUnixTimeMs || t < minimalUnixTimeMs {
		return fmt.Errorf("time '%d' doesn't seem to be a valid Unix timesamp in milliseconds. The time must be in the years range 1970-2400", t)
	}
	return nil
}
func (mc *MetricsCache) Close() {
	//for 3 go funcs
	mc.stopChan <- 0
	mc.stopChan <- 0
	mc.stopChan <- 0
}

func (mc *MetricsCache) WaitForCompletion(timeout time.Duration) (int, error) {
	waitChan := make(chan int, 2)
	mc.asyncAppendChan <- &asyncAppend{metric: nil, t: 0, v: 0, resp: waitChan}

	var maxWaitTime time.Duration

	if timeout == 0 {
		maxWaitTime = 24 * time.Hour // Almost-infinite time
	} else if timeout > 0 {
		maxWaitTime = timeout
	} else {
		// If negative, use the default configured timeout value
		maxWaitTime = time.Duration(mc.cfg.DefaultTimeoutInSeconds) * time.Second
	}

	var resultCount int
	var err error

	mc.performanceReporter.WithTimer("WaitForCompletionTimer", func() {
		select {
		case resultCount = <-waitChan:
			err = mc.lastError
			mc.lastError = nil
			return
		case <-time.After(maxWaitTime):
			resultCount = 0
			err = errors.Errorf("The operation timed out after %.2f seconds.", maxWaitTime.Seconds())
			return
		}
	})

	return resultCount, err
}
