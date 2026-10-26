// Package billing holds message types the way an in-house library's data packages do: each
// type names its topic through a Topic() method returning a constant.
package billing

import (
	"errors"

	"example.com/inhouse/kafkabus/v2/types"
)

const (
	TopicInvoiceRequested types.Topic = types.Topic("billing.invoice.requested")
	TopicInvoiceIssued    types.Topic = types.Topic("billing.invoice.issued")
	TopicInvoicePaid      types.Topic = types.Topic("billing.invoice.paid")
	TopicReceipt          types.Topic = types.Topic("billing.receipts")
	TopicPaymentRecorded  types.Topic = types.Topic("billing.payment.recorded")
)

type DataInvoiceRequested struct {
	Account string
	Amount  int64
}

func (DataInvoiceRequested) Topic() types.Topic { return TopicInvoiceRequested }

func (d DataInvoiceRequested) Validate() error {
	if d.Account == "" {
		return errors.New("billing: empty account")
	}
	return nil
}

type DataInvoiceIssued struct{ InvoiceID string }

func (DataInvoiceIssued) Topic() types.Topic { return TopicInvoiceIssued }

func (DataInvoiceIssued) Validate() error { return nil }

type DataPaymentRecorded struct{ InvoiceID string }

func (DataPaymentRecorded) Topic() types.Topic { return TopicPaymentRecorded }

func (DataPaymentRecorded) Validate() error { return nil }

type DataReceipt struct{ InvoiceID string }

func (*DataReceipt) Topic() types.Topic { return TopicReceipt }

func (*DataReceipt) Validate() error { return nil }
