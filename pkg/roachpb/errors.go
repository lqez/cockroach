// Copyright 2014 The Cockroach Authors.
//
// Use of this software is governed by the Business Source License
// included in the file licenses/BSL.txt.
//
// As of the Change Date specified in that file, in accordance with
// the Business Source License, use of this software will be governed
// by the Apache License, Version 2.0, included in the file
// licenses/APL.txt.

package roachpb

import (
	"bytes"
	"errors"
	"fmt"
	"strings"

	"github.com/cockroachdb/cockroach/pkg/util/caller"
	"github.com/cockroachdb/cockroach/pkg/util/hlc"
	"github.com/cockroachdb/cockroach/pkg/util/uuid"
)

// ClientVisibleRetryError is to be implemented by errors visible by
// layers above and that can be handled by retrying the transaction.
type ClientVisibleRetryError interface {
	ClientVisibleRetryError()
}

// ClientVisibleAmbiguousError is to be implemented by errors visible
// by layers above and that indicate uncertainty.
type ClientVisibleAmbiguousError interface {
	ClientVisibleAmbiguousError()
}

func (e *UnhandledRetryableError) Error() string {
	return e.PErr.Message
}

var _ error = &UnhandledRetryableError{}

// ErrorUnexpectedlySet creates a string to panic with when a response (typically
// a roachpb.BatchResponse) unexpectedly has Error set in its response header.
func ErrorUnexpectedlySet(culprit, response interface{}) string {
	return fmt.Sprintf("error is unexpectedly set, culprit is %T:\n%+v", culprit, response)
}

// transactionRestartError is an interface implemented by errors that cause
// a transaction to be restarted.
type transactionRestartError interface {
	canRestartTransaction() TransactionRestart
}

// NewError creates an Error from the given error.
func NewError(err error) *Error {
	if err == nil {
		return nil
	}
	e := &Error{}
	e.SetDetail(err)
	return e
}

// NewErrorWithTxn creates an Error from the given error and a transaction.
//
// txn is cloned before being stored in Error.
func NewErrorWithTxn(err error, txn *Transaction) *Error {
	e := NewError(err)
	e.SetTxn(txn)
	return e
}

// NewErrorf creates an Error from the given error message. It is a
// passthrough to fmt.Errorf, with an additional prefix containing the
// filename and line number.
func NewErrorf(format string, a ...interface{}) *Error {
	// Cannot use errors.Errorf here due to cyclic dependency.
	file, line, _ := caller.Lookup(1)
	s := fmt.Sprintf("%s:%d: ", file, line)
	return NewError(fmt.Errorf(s+format, a...))
}

// String implements fmt.Stringer.
func (e *Error) String() string {
	if e == nil {
		return "<nil>"
	}
	return e.Message
}

type internalError Error

func (e *internalError) Error() string {
	return (*Error)(e).String()
}

func (e *internalError) message(_ *Error) string {
	return (*Error)(e).String()
}

func (e *internalError) canRestartTransaction() TransactionRestart {
	return e.TransactionRestart
}

var _ ErrorDetailInterface = &internalError{}

// ErrorDetailInterface is an interface for each error detail.
type ErrorDetailInterface interface {
	error
	// message returns an error message.
	message(*Error) string
}

// GoError returns a Go error converted from Error.
func (e *Error) GoError() error {
	if e == nil {
		return nil
	}

	if e.TransactionRestart != TransactionRestart_NONE {
		return &UnhandledRetryableError{
			PErr: *e,
		}
	}
	return e.GetDetail()
}

// SetDetail sets the error detail for the error. The argument cannot be nil.
func (e *Error) SetDetail(err error) {
	if err == nil {
		panic("nil err argument")
	}
	if intErr, ok := err.(*internalError); ok {
		*e = *(*Error)(intErr)
	} else {
		if sErr, ok := err.(ErrorDetailInterface); ok {
			e.Message = sErr.message(e)
		} else {
			e.Message = err.Error()
		}
		var isTxnError bool
		if r, ok := err.(transactionRestartError); ok {
			isTxnError = true
			e.TransactionRestart = r.canRestartTransaction()
		}
		// If the specific error type exists in the detail union, set it.
		if !e.Detail.SetInner(err) {
			if _, isInternalError := err.(*internalError); !isInternalError && isTxnError {
				panic(fmt.Sprintf("transactionRestartError %T must be an ErrorDetail", err))
			}
		}
	}
}

// GetDetail returns an error detail associated with the error.
func (e *Error) GetDetail() ErrorDetailInterface {
	if e == nil {
		return nil
	}
	if err, ok := e.Detail.GetInner().(ErrorDetailInterface); ok {
		return err
	}
	// Unknown error detail; return the generic error.
	return (*internalError)(e)
}

// SetTxn sets the txn and resets the error message. txn is cloned before being
// stored in the Error.
func (e *Error) SetTxn(txn *Transaction) {
	e.UnexposedTxn = txn
	if txn != nil {
		e.UnexposedTxn = txn.Clone()
	}
	if sErr, ok := e.Detail.GetInner().(ErrorDetailInterface); ok {
		// Refresh the message as the txn is updated.
		e.Message = sErr.message(e)
	}
}

// GetTxn returns the txn.
func (e *Error) GetTxn() *Transaction {
	if e == nil {
		return nil
	}
	return e.UnexposedTxn
}

// UpdateTxn updates the error transaction.
func (e *Error) UpdateTxn(o *Transaction) {
	if o == nil {
		return
	}
	if e.UnexposedTxn == nil {
		e.UnexposedTxn = o.Clone()
	} else {
		e.UnexposedTxn.Update(o)
	}
}

// SetErrorIndex sets the index of the error.
func (e *Error) SetErrorIndex(index int32) {
	e.Index = &ErrPosition{Index: index}
}

func (e *NodeUnavailableError) Error() string {
	return e.message(nil)
}

func (*NodeUnavailableError) message(_ *Error) string {
	return "node unavailable; try another peer"
}

var _ ErrorDetailInterface = &NodeUnavailableError{}

func (e *NotLeaseHolderError) Error() string {
	return e.message(nil)
}

func (e *NotLeaseHolderError) message(_ *Error) string {
	const prefix = "[NotLeaseHolderError] "
	if e.CustomMsg != "" {
		return prefix + e.CustomMsg
	}
	if e.LeaseHolder == nil {
		return fmt.Sprintf("%sr%d: replica %s not lease holder; lease holder unknown", prefix, e.RangeID, e.Replica)
	} else if e.Lease != nil {
		return fmt.Sprintf("%sr%d: replica %s not lease holder; current lease is %s", prefix, e.RangeID, e.Replica, e.Lease)
	}
	return fmt.Sprintf("%sr%d: replica %s not lease holder; replica %s is", prefix, e.RangeID, e.Replica, *e.LeaseHolder)
}

var _ ErrorDetailInterface = &NotLeaseHolderError{}

func (e *LeaseRejectedError) Error() string {
	return e.message(nil)
}

func (e *LeaseRejectedError) message(_ *Error) string {
	return fmt.Sprintf("cannot replace lease %s with %s: %s", e.Existing, e.Requested, e.Message)
}

var _ ErrorDetailInterface = &LeaseRejectedError{}

// NewSendError creates a SendError.
func NewSendError(msg string) *SendError {
	return &SendError{Message: msg}
}

func (s SendError) Error() string {
	return s.message(nil)
}

func (s *SendError) message(_ *Error) string {
	return "failed to send RPC: " + s.Message
}

var _ ErrorDetailInterface = &SendError{}

// NewRangeNotFoundError initializes a new RangeNotFoundError for the given RangeID and, optionally,
// a StoreID.
func NewRangeNotFoundError(rangeID RangeID, storeID StoreID) *RangeNotFoundError {
	return &RangeNotFoundError{
		RangeID: rangeID,
		StoreID: storeID,
	}
}

func (e *RangeNotFoundError) Error() string {
	return e.message(nil)
}

func (e *RangeNotFoundError) message(_ *Error) string {
	msg := fmt.Sprintf("r%d was not found", e.RangeID)
	if e.StoreID != 0 {
		msg += fmt.Sprintf(" on s%d", e.StoreID)
	}
	return msg
}

var _ ErrorDetailInterface = &RangeNotFoundError{}

// NewRangeKeyMismatchError initializes a new RangeKeyMismatchError.
func NewRangeKeyMismatchError(start, end Key, desc *RangeDescriptor) *RangeKeyMismatchError {
	if desc != nil && !desc.IsInitialized() {
		// We must never send uninitialized ranges back to the client (nil
		// is fine) guard against regressions of #6027.
		panic(fmt.Sprintf("descriptor is not initialized: %+v", desc))
	}
	return &RangeKeyMismatchError{
		RequestStartKey: start,
		RequestEndKey:   end,
		MismatchedRange: desc,
	}
}

func (e *RangeKeyMismatchError) Error() string {
	return e.message(nil)
}

func (e *RangeKeyMismatchError) message(_ *Error) string {
	if e.MismatchedRange != nil {
		return fmt.Sprintf("key range %s-%s outside of bounds of range %s-%s",
			e.RequestStartKey, e.RequestEndKey, e.MismatchedRange.StartKey, e.MismatchedRange.EndKey)
	}
	return fmt.Sprintf("key range %s-%s could not be located within a range on store", e.RequestStartKey, e.RequestEndKey)
}

var _ ErrorDetailInterface = &RangeKeyMismatchError{}

// NewAmbiguousResultError initializes a new AmbiguousResultError with
// an explanatory message.
func NewAmbiguousResultError(msg string) *AmbiguousResultError {
	return &AmbiguousResultError{Message: msg}
}

func (e *AmbiguousResultError) Error() string {
	return e.message(nil)
}

func (e *AmbiguousResultError) message(_ *Error) string {
	return fmt.Sprintf("result is ambiguous (%s)", e.Message)
}

// ClientVisibleAmbiguousError implements the ClientVisibleAmbiguousError interface.
func (e *AmbiguousResultError) ClientVisibleAmbiguousError() {}

var _ ErrorDetailInterface = &AmbiguousResultError{}
var _ ClientVisibleAmbiguousError = &AmbiguousResultError{}

func (e *TransactionAbortedError) Error() string {
	return fmt.Sprintf("TransactionAbortedError(%s)", e.Reason)
}

func (e *TransactionAbortedError) message(pErr *Error) string {
	return fmt.Sprintf("TransactionAbortedError(%s): %s", e.Reason, pErr.GetTxn())
}

func (*TransactionAbortedError) canRestartTransaction() TransactionRestart {
	return TransactionRestart_IMMEDIATE
}

var _ ErrorDetailInterface = &TransactionAbortedError{}
var _ transactionRestartError = &TransactionAbortedError{}

// ClientVisibleRetryError implements the ClientVisibleRetryError interface.
func (e *TransactionRetryWithProtoRefreshError) ClientVisibleRetryError() {}

func (e *TransactionRetryWithProtoRefreshError) Error() string {
	return e.message(nil)
}

func (e *TransactionRetryWithProtoRefreshError) message(_ *Error) string {
	return fmt.Sprintf("TransactionRetryWithProtoRefreshError: %s", e.Msg)
}

var _ ClientVisibleRetryError = &TransactionRetryWithProtoRefreshError{}
var _ ErrorDetailInterface = &TransactionRetryWithProtoRefreshError{}

// NewTransactionAbortedError initializes a new TransactionAbortedError.
func NewTransactionAbortedError(reason TransactionAbortedReason) *TransactionAbortedError {
	return &TransactionAbortedError{
		Reason: reason,
	}
}

// NewTransactionRetryWithProtoRefreshError initializes a new TransactionRetryWithProtoRefreshError.
//
// txnID is the ID of the transaction being restarted.
// txn is the transaction that the client should use for the next attempts.
func NewTransactionRetryWithProtoRefreshError(
	msg string, txnID uuid.UUID, txn Transaction,
) *TransactionRetryWithProtoRefreshError {
	return &TransactionRetryWithProtoRefreshError{
		Msg:         msg,
		TxnID:       txnID,
		Transaction: txn,
	}
}

// PrevTxnAborted returns true if this error originated from a
// TransactionAbortedError. If true, the client will need to create a new
// transaction, as opposed to continuing with the existing one at a bumped
// epoch.
func (e *TransactionRetryWithProtoRefreshError) PrevTxnAborted() bool {
	return !e.TxnID.Equal(e.Transaction.ID)
}

// NewTransactionPushError initializes a new TransactionPushError.
func NewTransactionPushError(pusheeTxn Transaction) *TransactionPushError {
	// Note: this error will cause a txn restart. The error that the client
	// receives contains a txn that might have a modified priority.
	return &TransactionPushError{PusheeTxn: pusheeTxn}
}

func (e *TransactionPushError) Error() string {
	return e.message(nil)
}

func (e *TransactionPushError) message(pErr *Error) string {
	s := fmt.Sprintf("failed to push %s", e.PusheeTxn)
	if pErr.GetTxn() == nil {
		return s
	}
	return fmt.Sprintf("txn %s %s", pErr.GetTxn(), s)
}

var _ ErrorDetailInterface = &TransactionPushError{}
var _ transactionRestartError = &TransactionPushError{}

func (*TransactionPushError) canRestartTransaction() TransactionRestart {
	return TransactionRestart_IMMEDIATE
}

// NewTransactionRetryError initializes a new TransactionRetryError.
func NewTransactionRetryError(
	reason TransactionRetryReason, extraMsg string,
) *TransactionRetryError {
	return &TransactionRetryError{
		Reason:   reason,
		ExtraMsg: extraMsg,
	}
}

func (e *TransactionRetryError) Error() string {
	return fmt.Sprintf("TransactionRetryError: retry txn (%s)", e.Reason)
}

func (e *TransactionRetryError) message(pErr *Error) string {
	return fmt.Sprintf("%s: %s", e.Error(), pErr.GetTxn())
}

var _ ErrorDetailInterface = &TransactionRetryError{}
var _ transactionRestartError = &TransactionRetryError{}

func (*TransactionRetryError) canRestartTransaction() TransactionRestart {
	return TransactionRestart_IMMEDIATE
}

// NewTransactionStatusError initializes a new TransactionStatusError from
// the given message.
func NewTransactionStatusError(msg string) *TransactionStatusError {
	return &TransactionStatusError{
		Msg:    msg,
		Reason: TransactionStatusError_REASON_UNKNOWN,
	}
}

// NewTransactionCommittedStatusError initializes a new TransactionStatusError
// with a REASON_TXN_COMMITTED.
func NewTransactionCommittedStatusError() *TransactionStatusError {
	return &TransactionStatusError{
		Msg:    "already committed",
		Reason: TransactionStatusError_REASON_TXN_COMMITTED,
	}
}

func (e *TransactionStatusError) Error() string {
	return fmt.Sprintf("TransactionStatusError: %s (%s)", e.Msg, e.Reason)
}

func (e *TransactionStatusError) message(pErr *Error) string {
	return fmt.Sprintf("%s: %s", e.Error(), pErr.GetTxn())
}

var _ ErrorDetailInterface = &TransactionStatusError{}

func (e *WriteIntentError) Error() string {
	return e.message(nil)
}

func (e *WriteIntentError) message(_ *Error) string {
	var buf bytes.Buffer
	buf.WriteString("conflicting intents on ")

	// If we have a lot of intents, we only want to show the first and the last.
	const maxBegin = 5
	const maxEnd = 5
	var begin, end []Intent
	if len(e.Intents) <= maxBegin+maxEnd {
		begin = e.Intents
	} else {
		begin = e.Intents[0:maxBegin]
		end = e.Intents[len(e.Intents)-maxEnd : len(e.Intents)]
	}

	for i := range begin {
		if i > 0 {
			buf.WriteString(", ")
		}
		buf.WriteString(begin[i].Key.String())
	}
	if end != nil {
		buf.WriteString(" ... ")
		for i := range end {
			if i > 0 {
				buf.WriteString(", ")
			}
			buf.WriteString(end[i].Key.String())
		}
	}
	return buf.String()
}

var _ ErrorDetailInterface = &WriteIntentError{}

func (e *WriteTooOldError) Error() string {
	return e.message(nil)
}

func (e *WriteTooOldError) message(_ *Error) string {
	return fmt.Sprintf("WriteTooOldError: write at timestamp %s too old; wrote at %s",
		e.Timestamp, e.ActualTimestamp)
}

var _ ErrorDetailInterface = &WriteTooOldError{}
var _ transactionRestartError = &WriteTooOldError{}

func (*WriteTooOldError) canRestartTransaction() TransactionRestart {
	return TransactionRestart_IMMEDIATE
}

// NewReadWithinUncertaintyIntervalError creates a new uncertainty retry error.
// The read and existing timestamps as well as the txn are purely informational
// and used for formatting the error message.
func NewReadWithinUncertaintyIntervalError(
	readTS, existingTS hlc.Timestamp, txn *Transaction,
) *ReadWithinUncertaintyIntervalError {
	rwue := &ReadWithinUncertaintyIntervalError{
		ReadTimestamp:     readTS,
		ExistingTimestamp: existingTS,
	}
	if txn != nil {
		maxTS := txn.MaxTimestamp
		rwue.MaxTimestamp = &maxTS
		rwue.ObservedTimestamps = txn.ObservedTimestamps
	}
	return rwue
}

func (e *ReadWithinUncertaintyIntervalError) Error() string {
	return e.message(nil)
}

func (e *ReadWithinUncertaintyIntervalError) message(_ *Error) string {
	var ts strings.Builder
	ts.WriteByte('[')
	for i, ot := range observedTimestampSlice(e.ObservedTimestamps) {
		if i > 0 {
			ts.WriteByte(' ')
		}
		fmt.Fprintf(&ts, "{%d %v}", ot.NodeID, ot.Timestamp)
	}
	ts.WriteByte(']')

	return fmt.Sprintf("ReadWithinUncertaintyIntervalError: read at time %s encountered "+
		"previous write with future timestamp %s within uncertainty interval `t <= %v`; "+
		"observed timestamps: %s",
		e.ReadTimestamp, e.ExistingTimestamp, e.MaxTimestamp, ts.String())
}

var _ ErrorDetailInterface = &ReadWithinUncertaintyIntervalError{}
var _ transactionRestartError = &ReadWithinUncertaintyIntervalError{}

func (*ReadWithinUncertaintyIntervalError) canRestartTransaction() TransactionRestart {
	return TransactionRestart_IMMEDIATE
}

func (e *OpRequiresTxnError) Error() string {
	return e.message(nil)
}

func (e *OpRequiresTxnError) message(_ *Error) string {
	return "the operation requires transactional context"
}

var _ ErrorDetailInterface = &OpRequiresTxnError{}

func (e *ConditionFailedError) Error() string {
	return e.message(nil)
}

func (e *ConditionFailedError) message(_ *Error) string {
	return fmt.Sprintf("unexpected value: %s", e.ActualValue)
}

var _ ErrorDetailInterface = &ConditionFailedError{}

func (e *RaftGroupDeletedError) Error() string {
	return e.message(nil)
}

func (*RaftGroupDeletedError) message(_ *Error) string {
	return "raft group deleted"
}

var _ ErrorDetailInterface = &RaftGroupDeletedError{}

// NewReplicaCorruptionError creates a new error indicating a corrupt replica.
// The supplied error is used to provide additional detail in the error message.
func NewReplicaCorruptionError(err error) *ReplicaCorruptionError {
	return &ReplicaCorruptionError{ErrorMsg: err.Error()}
}

func (e *ReplicaCorruptionError) Error() string {
	return e.message(nil)
}

func (e *ReplicaCorruptionError) message(_ *Error) string {
	msg := fmt.Sprintf("replica corruption (processed=%t)", e.Processed)
	if e.ErrorMsg != "" {
		msg += ": " + e.ErrorMsg
	}
	return msg
}

var _ ErrorDetailInterface = &ReplicaCorruptionError{}

// NewReplicaTooOldError initializes a new ReplicaTooOldError.
func NewReplicaTooOldError(replicaID ReplicaID) *ReplicaTooOldError {
	return &ReplicaTooOldError{
		ReplicaID: replicaID,
	}
}

func (e *ReplicaTooOldError) Error() string {
	return e.message(nil)
}

func (*ReplicaTooOldError) message(_ *Error) string {
	return "sender replica too old, discarding message"
}

var _ ErrorDetailInterface = &ReplicaTooOldError{}

// NewStoreNotFoundError initializes a new StoreNotFoundError.
func NewStoreNotFoundError(storeID StoreID) *StoreNotFoundError {
	return &StoreNotFoundError{
		StoreID: storeID,
	}
}

func (e *StoreNotFoundError) Error() string {
	return e.message(nil)
}

func (e *StoreNotFoundError) message(_ *Error) string {
	return fmt.Sprintf("store %d was not found", e.StoreID)
}

var _ ErrorDetailInterface = &StoreNotFoundError{}

func (e *TxnAlreadyEncounteredErrorError) Error() string {
	return e.message(nil)
}

func (e *TxnAlreadyEncounteredErrorError) message(_ *Error) string {
	return fmt.Sprintf(
		"txn already encountered an error; cannot be used anymore (previous err: %s)",
		e.PrevError,
	)
}

var _ ErrorDetailInterface = &TxnAlreadyEncounteredErrorError{}

func (e *IntegerOverflowError) Error() string {
	return e.message(nil)
}

func (e *IntegerOverflowError) message(_ *Error) string {
	return fmt.Sprintf(
		"key %s with value %d incremented by %d results in overflow",
		e.Key, e.CurrentValue, e.IncrementValue)
}

var _ ErrorDetailInterface = &IntegerOverflowError{}

func (e *UnsupportedRequestError) Error() string {
	return e.message(nil)
}

func (e *UnsupportedRequestError) message(_ *Error) string {
	return "unsupported request"
}

var _ ErrorDetailInterface = &UnsupportedRequestError{}

// WrapWithMixedSuccessError creates a new MixedSuccessError that wraps the
// provided error detail. If the detail is already a MixedSuccessError then
// no wrapping is performed.
func WrapWithMixedSuccessError(detail error) *MixedSuccessError {
	if m, ok := detail.(*MixedSuccessError); ok {
		return m
	}
	var m MixedSuccessError
	if !m.Wrapped.SetInner(detail) {
		// If the detail was not an ErrorDetail, store
		// it in the unstructured Message field.
		m.WrappedMessage = detail.Error()
	}
	return &m
}

// GetWrapped returns the error that the MixedSuccessError wraps.
func (e *MixedSuccessError) GetWrapped() error {
	if w := e.Wrapped.GetInner(); w != nil {
		return w
	}
	return errors.New(e.WrappedMessage)
}

func (e *MixedSuccessError) Error() string {
	return e.message(nil)
}

func (e *MixedSuccessError) message(_ *Error) string {
	return fmt.Sprintf("the batch experienced mixed success and failure: %s", e.GetWrapped())
}

var _ ErrorDetailInterface = &MixedSuccessError{}

func (e *BatchTimestampBeforeGCError) Error() string {
	return e.message(nil)
}

func (e *BatchTimestampBeforeGCError) message(_ *Error) string {
	return fmt.Sprintf("batch timestamp %v must be after replica GC threshold %v", e.Timestamp, e.Threshold)
}

var _ ErrorDetailInterface = &BatchTimestampBeforeGCError{}

// NewIntentMissingError creates a new IntentMissingError.
func NewIntentMissingError(key Key, wrongIntent *Intent) *IntentMissingError {
	return &IntentMissingError{
		Key:         key,
		WrongIntent: wrongIntent,
	}
}

func (e *IntentMissingError) Error() string {
	return e.message(nil)
}

func (e *IntentMissingError) message(_ *Error) string {
	var detail string
	if e.WrongIntent != nil {
		detail = fmt.Sprintf("; found intent %v at key instead", e.WrongIntent)
	}
	return fmt.Sprintf("intent missing%s", detail)
}

var _ ErrorDetailInterface = &IntentMissingError{}

func (e *MergeInProgressError) Error() string {
	return e.message(nil)
}

func (e *MergeInProgressError) message(_ *Error) string {
	return "merge in progress"
}

var _ ErrorDetailInterface = &MergeInProgressError{}

// NewRangeFeedRetryError initializes a new RangeFeedRetryError.
func NewRangeFeedRetryError(reason RangeFeedRetryError_Reason) *RangeFeedRetryError {
	return &RangeFeedRetryError{
		Reason: reason,
	}
}

func (e *RangeFeedRetryError) Error() string {
	return e.message(nil)
}

func (e *RangeFeedRetryError) message(pErr *Error) string {
	return fmt.Sprintf("retry rangefeed (%s)", e.Reason)
}

var _ ErrorDetailInterface = &RangeFeedRetryError{}

// NewIndeterminateCommitError initializes a new IndeterminateCommitError.
func NewIndeterminateCommitError(txn Transaction) *IndeterminateCommitError {
	return &IndeterminateCommitError{StagingTxn: txn}
}

func (e *IndeterminateCommitError) Error() string {
	return e.message(nil)
}

func (e *IndeterminateCommitError) message(pErr *Error) string {
	s := fmt.Sprintf("found txn in indeterminate STAGING state %s", e.StagingTxn)
	if pErr.GetTxn() == nil {
		return s
	}
	return fmt.Sprintf("txn %s %s", pErr.GetTxn(), s)
}

var _ ErrorDetailInterface = &IndeterminateCommitError{}
