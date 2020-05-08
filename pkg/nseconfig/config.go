package nseconfig

import (
	"strings"
)

type config interface {
	validate() error
}

type Config struct {
	Endpoints []*Endpoint
}

type Endpoint struct {
	Name   string
	Labels Labels

	NseName string //TODO temporary in order to be able to run examples

	CNNS CNNS

	VL3 VL3
}

type CNNS struct {
	Name               string
	Address            string
	AccessToken        string
	ConnectivityDomain string
}

type VL3 struct {
	IPAM        IPAM
	Ifname      string
	NameServers []string
}

type IPAM struct {
	PrefixPool   string
	PrefixLength int
	Routes       []string
}

type decoder interface {
	Decode(v interface{}) error
}

type DecoderFn func(v interface{}) error

func (d DecoderFn) Decode(v interface{}) error { return d(v) }

func NewConfig(decoder decoder, cfg config) error {
	if err := decoder.Decode(cfg); err != nil {
		return err
	}

	return cfg.validate()
}

func empty(s string) bool {
	return len(strings.Trim(s, " ")) == 0
}
