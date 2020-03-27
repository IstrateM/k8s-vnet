package nseconfig

import (
	"bytes"
	"fmt"
	"net"
	"testing"

	"gopkg.in/yaml.v3"
	"gotest.tools/assert"
)

func TestNewConfig(t *testing.T) {
	for name, tc := range map[string]struct {
		file   string
		config *Config
		err    error
	}{
		"success": {
			file: testFile1,
			config: &Config{Endpoints: []*Endpoint{{CNNS: CNNS{
				Name:        "cnns1",
				Address:     "golang.com:9000",
				AccessToken: "123123",
			}, VL3: VL3{
				IPAM: IPAM{
					PrefixPool: "192.168.33.0/24",
					Routes:     []string{"192.168.34.0/24"},
				},
				Ifname:      "nsm3",
				NameServers: []string{"nms.google.com", "nms.google.com2"},
			}}}},
		},
		"validation-errors": {
			file: testFile2,
			err: InvalidConfigErrors([]error{
				fmt.Errorf("cnns addreses is not set"),
				fmt.Errorf("cnns name is not set"),
				fmt.Errorf("prefix pool is not a valid subnet: %s", &net.ParseError{Type: "CIDR address", Text: "invalid-pull"}),
				fmt.Errorf("route nr %d with value %s is not a valid subnet: %s", 0, "invalid-route1", &net.ParseError{Type: "CIDR address", Text: "invalid-route1"}),
				fmt.Errorf("route nr %d with value %s is not a valid subnet: %s", 1, "invalid-route2", &net.ParseError{Type: "CIDR address", Text: "invalid-route2"}),
			}),
		},
	} {
		t.Run(name, func(t *testing.T) {
			cfg := &Config{}
			err := NewConfig(yaml.NewDecoder(bytes.NewBufferString(tc.file)), cfg)
			if tc.err != nil {
				if err == nil {
					t.Fatal("error should not be empty")
				}

				assert.Equal(t, tc.err.Error(), err.Error())
			} else {
				assert.DeepEqual(t, tc.config, cfg)
			}
		})
	}
}

const testFile1 = `
endpoints:
  - cnns:
      name: cnns1
      address: golang.com:9000
      accesstoken: 123123
    vl3:
      ipam:
        prefixpool: 192.168.33.0/24
        routes: [192.168.34.0/24]
      ifname: nsm3
      nameservers: [nms.google.com, nms.google.com2]
`

const testFile2 = `
endpoints:
  - cnns:
      name: ""
      address: ""
    vl3:
      ipam:
        prefixpool: invalid-pull
        routes: [invalid-route1, invalid-route2]
      ifname: 
      nameservers: []
`
