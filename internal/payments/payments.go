// Package payments implements explicitly configured merchant adapters. Browser
// return pages are informational; only verified notifications or queries settle.
package payments

import (
	"context"
	"errors"
	"net/http"
)

var ErrVerification = errors.New("payment signature or merchant data invalid")
var ErrGateway = errors.New("payment gateway unavailable or response invalid")
var ErrNotPaid = errors.New("payment not completed")

type Request struct {
	PaymentID, OrderID, Name       string
	Amount                         int64
	Currency, NotifyURL, ReturnURL string
}
type Checkout struct {
	Kind string `json:"kind"` // redirect or qr
	URL  string `json:"url"`
}
type Notification struct {
	PaymentID, TransactionID string
	Amount                   int64
	Currency                 string
	Paid                     bool
}
type Provider interface {
	ID() string
	Name() string
	Create(context.Context, Request) (Checkout, error)
	Verify(*http.Request) (Notification, error)
	Query(context.Context, Request) (Notification, error)
}
type Method struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}
type Registry struct{ providers map[string]Provider }

func New(providers ...Provider) (*Registry, error) {
	r := &Registry{providers: make(map[string]Provider)}
	for _, p := range providers {
		if p == nil {
			return nil, errors.New("nil payment provider")
		}
		id := p.ID()
		if id != "alipay" && id != "wechat" && id != "epay_alipay" && id != "epay_wechat" {
			return nil, errors.New("unsupported payment provider")
		}
		if _, ok := r.providers[id]; ok {
			return nil, errors.New("duplicate payment provider")
		}
		r.providers[id] = p
	}
	return r, nil
}
func (r *Registry) Get(id string) (Provider, bool) {
	if r == nil {
		return nil, false
	}
	p, ok := r.providers[id]
	return p, ok
}
func (r *Registry) Methods() []Method {
	methods := []Method{}
	for _, id := range []string{"alipay", "wechat", "epay_alipay", "epay_wechat"} {
		if p, ok := r.Get(id); ok {
			methods = append(methods, Method{ID: id, Name: p.Name()})
		}
	}
	return methods
}
