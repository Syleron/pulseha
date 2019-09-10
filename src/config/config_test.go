package config

import (
	"fmt"
	"reflect"
	"sync"
	"testing"
)

func TestTestthings(t *testing.T)  {
 c := Config{}
	ty := reflect.TypeOf(c.Pulse)
	for i := 0; i < ty.NumField(); i++ {
		field := ty.Field(i)
		fmt.Println(field)
		fmt.Println(field.Tag.Get("json"))
		fmt.Println(field.Type)
		// Get the field tag value
		//tag := field.Tag.Get(tagName)
		//if tag == "" {
		//	continue
		//}
	}
	fmt.Println(ty)
	//ty.
}

func TestConfig_UpdateValue(t *testing.T) {
	type fields struct {
		Pulse   Local
		Groups  map[string][]string
		Nodes   map[string]Node
		Logging Logging
		Mutex   sync.Mutex
	}

	type args struct {
		key   string
		value string
	}
	tests := []struct {
		name    string
		fields  fields
		args    args
		wantErr bool
	}{
		{
			name: "test1",
			fields:fields{
				Pulse:   Local{
					TLS:                 false,
					HealthCheckInterval: 0,
					FailOverInterval:    0,
					FailOverLimit:       0,
					LocalNode:           "",
				},
				Groups:  nil,
				Nodes:   nil,
				Logging: Logging{},
				Mutex:   sync.Mutex{},
			},
			args:args{
				key:   "",
				value: "",
			},
			},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := &Config{
				Pulse:   tt.fields.Pulse,
				Groups:  tt.fields.Groups,
				Nodes:   tt.fields.Nodes,
				Logging: tt.fields.Logging,
				Mutex:   tt.fields.Mutex,
			}
			if err := c.UpdateValue(tt.args.key, tt.args.value); (err != nil) != tt.wantErr {
				t.Errorf("UpdateValue() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}