// Copyright 2018 The Cockroach Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or
// implied. See the License for the specific language governing
// permissions and limitations under the License.

package telemetry

import (
	"fmt"
	"sync/atomic"

	"github.com/cockroachdb/cockroach/pkg/sql/pgwire/pgerror"
	"github.com/cockroachdb/cockroach/pkg/util/log"
	"github.com/cockroachdb/cockroach/pkg/util/metric"
	"github.com/cockroachdb/cockroach/pkg/util/syncutil"
)

// Bucket10 buckets a number by order of magnitude base 10, eg 637 -> 100.
// This can be used in telemetry to get ballpark ideas of how users use a given
// feature, such as file sizes, qps, etc, without being as revealing as the
// raw numbers.
// The numbers 0-10 are reported unchanged.
func Bucket10(num int64) int64 {
	if num <= 0 {
		return 0
	}
	if num < 10 {
		return num
	}
	res := int64(10)
	for ; res < 1000000000000000000 && res*10 < num; res *= 10 {
	}
	return res
}

// CountBucketed counts the feature identified by prefix and the value, using
// the bucketed value to pick a feature bucket to increment, e.g. a prefix of
// "foo.bar" and value of 632 would be counted as "foo.bar.100".
func CountBucketed(prefix string, value int64) {
	Count(fmt.Sprintf("%s.%d", prefix, Bucket10(value)))
}

// Count retrieves and increments the usage counter for the passed feature.
// High-volume callers may want to instead use `GetCounter` and hold on to the
// returned Counter between calls to Inc, to avoid contention in the registry.
func Count(feature string) {
	Inc(GetCounter(feature))
}

// Counter represents the usage counter for a given 'feature'.
type Counter *int32

// Inc increments the counter.
func Inc(c Counter) {
	atomic.AddInt32(c, 1)
}

// GetCounterOnce returns a counter from the global registry,
// and asserts it didn't exist previously.
func GetCounterOnce(feature string) Counter {
	counters.RLock()
	_, ok := counters.m[feature]
	counters.RUnlock()
	if ok {
		panic("counter already exists: " + feature)
	}
	return GetCounter(feature)
}

// GetCounter returns a counter from the global registry.
func GetCounter(feature string) Counter {
	counters.RLock()
	i, ok := counters.m[feature]
	counters.RUnlock()

	if !ok {
		counters.Lock()
		var n int32
		counters.m[feature] = &n
		i = &n
		counters.Unlock()
	}
	return i
}

// CounterWithMetric combines a telemetry and a metrics counter.
type CounterWithMetric struct {
	telemetry Counter
	metric    *metric.Counter
}

// Necessary for metric metadata registration.
var _ metric.Iterable = CounterWithMetric{}

// NewCounterWithMetric creates a CounterWithMetric.
func NewCounterWithMetric(metadata metric.Metadata) CounterWithMetric {
	return CounterWithMetric{
		telemetry: GetCounter(metadata.Name),
		metric:    metric.NewCounter(metadata),
	}
}

// Inc increments both counters.
func (c CounterWithMetric) Inc() {
	Inc(c.telemetry)
	c.metric.Inc(1)
}

// Forward the metric.Iterable interface to the metric counter. We
// don't just embed the counter because our Inc() interface is a bit
// different.

// GetName implements metric.Iterable
func (c CounterWithMetric) GetName() string {
	return c.metric.GetName()
}

// GetHelp implements metric.Iterable
func (c CounterWithMetric) GetHelp() string {
	return c.metric.GetHelp()
}

// GetMeasurement implements metric.Iterable
func (c CounterWithMetric) GetMeasurement() string {
	return c.metric.GetMeasurement()
}

// GetUnit implements metric.Iterable
func (c CounterWithMetric) GetUnit() metric.Unit {
	return c.metric.GetUnit()
}

// GetMetadata implements metric.Iterable
func (c CounterWithMetric) GetMetadata() metric.Metadata {
	return c.metric.GetMetadata()
}

// Inspect implements metric.Iterable
func (c CounterWithMetric) Inspect(f func(interface{})) {
	c.metric.Inspect(f)
}

func init() {
	counters.m = make(map[string]Counter, approxFeatureCount)
}

var approxFeatureCount = 1500

// counters stores the registry of feature-usage counts.
// TODO(dt): consider a lock-free map.
var counters struct {
	syncutil.RWMutex
	m map[string]Counter
}

// QuantizeCounts controls if counts are quantized when fetched.
type QuantizeCounts bool

// ResetCounters controls if counts are reset when fetched.
type ResetCounters bool

const (
	// Quantized returns counts quantized to order of magnitude.
	Quantized QuantizeCounts = true
	// Raw returns the raw, unquanitzed counter values.
	Raw QuantizeCounts = false
	// ResetCounts resets the counter to zero after fetching its value.
	ResetCounts ResetCounters = true
	// ReadOnly leaves the counter value unchanged when reading it.
	ReadOnly ResetCounters = false
)

// GetRawFeatureCounts returns current raw, un-quanitzed feature counter values.
func GetRawFeatureCounts() map[string]int32 {
	return GetFeatureCounts(Raw, ReadOnly)
}

// GetFeatureCounts returns the current feature usage counts.
//
// It optionally quantizes quantizes the returned counts to just order of
// magnitude using the `Bucket10` helper, and optionally resets the counters to
// zero i.e. if flushing accumulated counts during a report.
func GetFeatureCounts(quantize QuantizeCounts, reset ResetCounters) map[string]int32 {
	counters.RLock()
	m := make(map[string]int32, len(counters.m))
	for k, cnt := range counters.m {
		var val int32
		if reset {
			val = atomic.SwapInt32(cnt, 0)
		} else {
			val = atomic.LoadInt32(cnt)
		}
		if val != 0 {
			m[k] = val
		}
	}
	counters.RUnlock()
	if quantize {
		for k := range m {
			m[k] = int32(Bucket10(int64(m[k])))
		}
	}
	return m
}

// RecordError takes an error and increments the corresponding count
// for its error code, and, if it is an unimplemented or internal
// error, the count for that feature or the internal error's shortened
// stack trace.
func RecordError(err error) {
	if err == nil {
		return
	}

	if pgErr, ok := pgerror.GetPGCause(err); ok {
		Count("errorcodes." + pgErr.Code)

		if details := pgErr.TelemetryKey; details != "" {
			var prefix string
			switch pgErr.Code {
			case pgerror.CodeFeatureNotSupportedError:
				prefix = "unimplemented."
			case pgerror.CodeInternalError:
				prefix = "internalerror."
			default:
				prefix = "othererror." + pgErr.Code + "."
			}
			Count(prefix + details)
		}
	} else {
		typ := log.ErrorSource(err)
		if typ == "" {
			typ = "unknown"
		}
		Count("othererror." + typ)
	}
}
