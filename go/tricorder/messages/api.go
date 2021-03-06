// Package messages provides the types needed to collect metrics via
// the go rpc calls and the REST API mentioned in the tricorder package.
package messages

import (
	"encoding/gob"
	"errors"
	"time"

	"github.com/Cloud-Foundations/tricorder/go/tricorder/types"
	"github.com/Cloud-Foundations/tricorder/go/tricorder/units"
)

var (
	// The MetricServer.GetMetric RPC call returns this if no
	// metric with given path exists.
	ErrMetricNotFound = errors.New("messages: No metric found.")
)

// RangeWithCount represents the number of values within a
// particular range.
type RangeWithCount struct {
	// Represents the lower bound of the range inclusive.
	// Ignore for the lowest range which never has a lower bound.
	Lower float64 `json:"lower"`
	// Represents the upper bound of the range exclusive.
	// Ignore for the highest range which never has a upper bound.
	Upper float64 `json:"upper"`
	// The number of values falling within the range.
	Count uint64 `json:"count"`
}

// Distribution represents a distribution of values.
type Distribution struct {
	// The minimum value
	Min float64 `json:"min"`
	// The maximum value
	Max float64 `json:"max"`
	// The average value
	Average float64 `json:"average"`
	// The approximate median value
	Median float64 `json:"median"`
	// The sum
	Sum float64 `json:"sum"`
	// The total number of values
	Count uint64 `json:"count"`
	// This field is incremented by 1 each time this distribution changes.
	Generation uint64 `json:"generation"`
	// This field is true if this distribution is not cumulative.
	IsNotCumulative bool `json:"isNotCumulative,omitempty"`
	// The number of values within each range
	Ranges []*RangeWithCount `json:"ranges,omitempty"`
}

func (d *Distribution) Type() types.Type {
	return types.Dist
}

// Metric represents a single metric
// The type of the actual value stored in the Value field depends on the
// value of the Kind field.
//
// The chart below lists what type Value contains for each value of the
// Kind and Subtype field:
//
//	Kind		SubType			Actual type
// 	types.Bool				bool
//	types.Int8				int8
//	types.Int16				int16
//	types.Int32				int32
//	types.Int64				int64
//	types.Uint8				uint8
//	types.Uint16				uint16
//	types.Uint32				uint32
//	types.Uint64				uint64
//	types.Float32				float32
//	types.Float64				float64
//	types.String				string
//	types.Dist				*messages.Distribution
//	types.Time				string: Seconds since Jan 1, 1970 GMT. 9 digits after the decimal point.
//	types.Duration				string: Seconds with 9 digits after the decimal point.
//	types.GoTime				time.Time (Go RPC only)
//	types.GoDuration			time.Duration (Go RPC only)
//	types.List	types.Bool		[]bool
//	types.List	types.Int8		[]int8
//	types.List	types.Int16		[]int16
//	types.List	types.Int32		[]int32
//	types.List	types.Int64		[]int64
//	types.List	types.Uint8		[]uint8
//	types.List	types.Uint16		[]uint16
//	types.List	types.Uint32		[]uint32
//	types.List	types.Uint64		[]uint64
//	types.List	types.Float32		[]float32
//	types.List	types.Float64		[]float64
//	types.List	types.String		[]string
//	types.List	types.Time		[]string
//	types.List	types.Duration		[]string
//	types.List	types.GoTime		[]time.Time (Go RPC only)
//	types.List	types.GoDuration	[]time.Duration (Go RPC only)
type Metric struct {
	// The absolute path to this metric
	Path string `json:"path"`
	// The description of this metric
	Description string `json:"description"`
	// The unit of measurement this metric represents
	Unit units.Unit `json:"unit,omitempty"`
	// The metric's type
	Kind types.Type `json:"kind"`
	// The sub-type if type is an aggregate such as List.
	SubType types.Type `json:"subType,omitempty"`
	// The size in bits of metric's value if Kind is
	// types.IntXX, types.UintXX, or types.FloatXX.
	Bits int `json:"bits,omitempty"`
	// value stored here
	Value interface{} `json:"value"`
	// TimeStamp of metric value.
	// For JSON this is seconds since Jan 1, 1970 as a string as
	// "1234567890.999999999"
	// For Go RPC this is a time.Time.
	// This is an optional field. In Go RPC, it can be nil; in JSON, it
	// can be the empty string.
	TimeStamp interface{} `json:"timestamp"`
	// GroupId of the metric's region. Metrics with the same group Id
	// will always have the same timestamp.
	GroupId int `json:"groupId"`
}

// ConvertToGoRPC changes this metric in place to be go rpc compatible.
func (m *Metric) ConvertToGoRPC() error {
	return m.convertToGoRPC()
}

// ConvertToJson changes this metric in place to be json compatible.
func (m *Metric) ConvertToJson() {
	m.convertToJson()
}

// MetricList represents a list of metrics. Clients should treat MetricList
// instances as immutable. In particular, clients should not modify contained
// Metric instances in place.
type MetricList []*Metric

// AsJsonWithSubType takes a metric value, kind, subtype, and unit and returns
// an acceptable JSON value, kind, and subType for given unit.
// If kind is types.List, subType indicates the type of elements in the list.
// Otherwise, caller should pass types.Unknown for subType. Likewise, if
// returned jsonKind is types.List, jsonSubType is the type of elements in
// the list. Otherwise jsonSubType is types.Unknown.
func AsJsonWithSubType(
	value interface{}, kind, subType types.Type, unit units.Unit) (
	jsonValue interface{}, jsonKind, jsonSubType types.Type) {
	return asJson(value, kind, subType, unit)
}

// ZeroValue works like types.Type.SafeZeroValue, but unlike the types.Type
// version, this method can also return zero values for types in this
// package.
func ZeroValue(t types.Type) (interface{}, error) {
	if t == types.Dist {
		return (*Distribution)(nil), nil
	} else {
		return t.SafeZeroValue()
	}
}

func init() {
	var tm time.Time
	var dur time.Duration
	var tml []time.Time
	var durl []time.Duration
	var dist *Distribution
	gob.Register(tm)
	gob.Register(dur)
	gob.Register(tml)
	gob.Register(durl)
	gob.Register(dist)
}
