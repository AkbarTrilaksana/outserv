// Portions Copyright 2016-2018 Dgraph Labs, Inc. are available under the Apache License v2.0.
// Portions Copyright 2022 Outcaste LLC are available under the Sustainable License v1.0.

package types

import (
	"math/big"
	"time"

	geom "github.com/twpayne/go-geom"
)

const nanoSecondsInSec = 1000000000
const dateFormatY = "2006" // time.longYear
const dateFormatYM = "2006-01"
const dateFormatYMD = "2006-01-02"
const dateFormatYMDZone = "2006-01-02 15:04:05 -0700 MST"
const dateTimeFormat = "2006-01-02T15:04:05"

var typeNameMap = map[string]TypeID{
	"default":  TypeDefault,
	"binary":   TypeBinary,
	"int":      TypeInt64,
	"float":    TypeFloat,
	"bool":     TypeBool,
	"datetime": TypeDatetime,
	"geo":      TypeGeo,
	"uid":      TypeUid,
	"string":   TypeString,
	"password": TypePassword,
	"upload":   TypeBinary,
	"bigint":   TypeBigInt,
}

// TypeID represents the type of the data.
type TypeID byte

const (
	TypeDefault TypeID = iota
	TypeBinary
	TypeInt64
	TypeFloat
	TypeBool
	TypeDatetime
	TypeGeo
	TypeUid
	TypePassword
	TypeString
	TypeObject
	TypeUndefined
	TypeBigInt
)

// Name returns the name of the type.
func (t TypeID) String() string {
	switch t {
	case TypeDefault:
		return "default"
	case TypeBinary:
		return "binary"
	case TypeInt64:
		return "int"
	case TypeFloat:
		return "float"
	case TypeBool:
		return "bool"
	case TypeDatetime:
		return "datetime"
	case TypeGeo:
		return "geo"
	case TypeUid:
		return "uid"
	case TypeString:
		return "string"
	case TypePassword:
		return "password"
	case TypeBigInt:
		return "bigint"
	}
	return ""
}

func (t TypeID) Int() int32 {
	return int32(t)
}

func FromInt(i int32) TypeID {
	if i < 0 || i >= int32(TypeUndefined) {
		return TypeUndefined
	}
	return TypeID(i)
}

// Serialized Value.
type Sval []byte

// Val is a value with type information.
type Val struct {
	Tid   TypeID
	Value interface{}
}

func (v Val) Marshal() ([]byte, error) {
	return ToBinary(v.Tid, v.Value)
}

// Safe ensures that Val's Value is not nil. This is useful when doing type
// assertions and default values might be involved.
// This function won't change the original v.Value, may it be nil.
// See: "Default value vars" in `fillVars()`
// Returns a safe v.Value suitable for type assertions.
func (v Val) Safe() interface{} {
	if v.Value == nil {
		// get zero value for this v.Tid
		va := ValueForType(v.Tid)
		return va.Value
	}
	return v.Value
}

// TypeForName returns the type corresponding to the given name.
// If name is not recognized, it returns nil.
func TypeForName(name string) (TypeID, bool) {
	t, ok := typeNameMap[name]
	return t, ok
}

// IsScalar returns whether the type is a scalar type.
func (t TypeID) IsScalar() bool {
	return t != TypeUid
}

// IsNumber returns whether the type is a number type.
func (t TypeID) IsNumber() bool {
	return t == TypeInt64 || t == TypeFloat
}

// ValueForType returns the zero value for a type id
func ValueForType(id TypeID) Val {
	switch id {
	case TypeBinary:
		var b []byte
		return Val{TypeBinary, &b}

	case TypeInt64:
		var i int64
		return Val{TypeInt64, &i}

	case TypeFloat:
		var f float64
		return Val{TypeFloat, &f}

	case TypeBool:
		var b bool
		return Val{TypeBool, &b}

	case TypeDatetime:
		var t time.Time
		return Val{TypeDatetime, &t}

	case TypeString:
		var s string
		return Val{TypeString, s}

	case TypeDefault:
		var s string
		return Val{TypeDefault, s}

	case TypeGeo:
		var g geom.T
		return Val{TypeGeo, &g}

	case TypeUid:
		var i uint64
		return Val{TypeUid, &i}

	case TypePassword:
		var p string
		return Val{TypePassword, p}

	case TypeBigInt:
		var i big.Int
		return Val{TypeBigInt, &i}

	default:
		return Val{}
	}
}

// ParseTime parses the time from string trying various datetime formats.
// By default, Go parses time in UTC unless specified in the data itself.
func ParseTime(val string) (time.Time, error) {
	if len(val) == len(dateFormatY) {
		return time.Parse(dateFormatY, val)
	}
	if len(val) == len(dateFormatYM) {
		return time.Parse(dateFormatYM, val)
	}
	if len(val) == len(dateFormatYMD) {
		return time.Parse(dateFormatYMD, val)
	}
	if len(val) > len(dateTimeFormat) && val[len(dateFormatYMD)] == 'T' &&
		(val[len(val)-1] == 'Z' || val[len(val)-3] == ':') {
		// https://tools.ietf.org/html/rfc3339#section-5.6
		return time.Parse(time.RFC3339, val)
	}
	if t, err := time.Parse(dateFormatYMDZone, val); err == nil {
		return t, err
	}
	// Try without timezone.
	return time.Parse(dateTimeFormat, val)
}
