package gocbcore

import "time"

// GetInOptions encapsulates the parameters for a GetInEx operation.
type GetInOptions struct {
	Key            []byte
	Path           string
	Flags          SubdocFlag
	CollectionName string
	ScopeName      string
	CollectionID   uint32
	RetryStrategy  RetryStrategy
	Deadline       time.Time

	// Volatile: Tracer API is subject to change.
	TraceContext RequestSpanContext
}

// ExistsInOptions encapsulates the parameters for a ExistsInEx operation.
type ExistsInOptions struct {
	Key            []byte
	Path           string
	Flags          SubdocFlag
	CollectionName string
	ScopeName      string
	CollectionID   uint32
	RetryStrategy  RetryStrategy
	Deadline       time.Time

	// Volatile: Tracer API is subject to change.
	TraceContext RequestSpanContext
}

// StoreInOptions encapsulates the parameters for a SetInEx, AddInEx, ReplaceInEx,
// PushFrontInEx, PushBackInEx, ArrayInsertInEx or AddUniqueInEx operation.
type StoreInOptions struct {
	Key                    []byte
	Path                   string
	Value                  []byte
	Flags                  SubdocFlag
	Cas                    Cas
	Expiry                 uint32
	CollectionName         string
	ScopeName              string
	RetryStrategy          RetryStrategy
	DurabilityLevel        DurabilityLevel
	DurabilityLevelTimeout uint16
	CollectionID           uint32
	Deadline               time.Time

	// Volatile: Tracer API is subject to change.
	TraceContext RequestSpanContext
}

// CounterInOptions encapsulates the parameters for a CounterInEx operation.
type CounterInOptions StoreInOptions

// DeleteInOptions encapsulates the parameters for a DeleteInEx operation.
type DeleteInOptions struct {
	Key                    []byte
	Path                   string
	Cas                    Cas
	Expiry                 uint32
	Flags                  SubdocFlag
	CollectionName         string
	ScopeName              string
	RetryStrategy          RetryStrategy
	DurabilityLevel        DurabilityLevel
	DurabilityLevelTimeout uint16
	CollectionID           uint32
	Deadline               time.Time

	// Volatile: Tracer API is subject to change.
	TraceContext RequestSpanContext
}

// LookupInOptions encapsulates the parameters for a LookupInEx operation.
type LookupInOptions struct {
	Key            []byte
	Flags          SubdocDocFlag
	Ops            []SubDocOp
	CollectionName string
	ScopeName      string
	CollectionID   uint32
	RetryStrategy  RetryStrategy
	Deadline       time.Time

	// Volatile: Tracer API is subject to change.
	TraceContext RequestSpanContext
}

// MutateInOptions encapsulates the parameters for a MutateInEx operation.
type MutateInOptions struct {
	Key                    []byte
	Flags                  SubdocDocFlag
	Cas                    Cas
	Expiry                 uint32
	Ops                    []SubDocOp
	CollectionName         string
	ScopeName              string
	RetryStrategy          RetryStrategy
	DurabilityLevel        DurabilityLevel
	DurabilityLevelTimeout uint16
	CollectionID           uint32
	Deadline               time.Time

	// Volatile: Tracer API is subject to change.
	TraceContext RequestSpanContext
}
