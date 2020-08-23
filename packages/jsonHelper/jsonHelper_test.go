package jsonHelper

import "testing"

type testStruct struct {
	Int    int    `json:"int"`
	Bool   bool   `json:"bool"`
	String string `json:"string"`
	TypeNotSet float32 `json:"float"`
}

func TestSetStructFieldByTag(t *testing.T) {
	type args struct {
		tag          string
		value        string
		taggedStruct interface{}
	}
	tests := []struct {
		name    string
		args    args
		wantErr bool
	}{
		{
			name: "testSetInt",
			args: args{
				tag:   "int",
				value: "5",
				taggedStruct: &testStruct{
					Int:    5,
					Bool:   false,
					String: "",
				},
			},
			wantErr: false,
		},
		{
			name: "testSetBool",
			args: args{
				tag:   "bool",
				value: "true",
				taggedStruct: &testStruct{
					Int:    0,
					Bool:   true,
					String: "",
				},
			},
			wantErr: false,
		},
		{
			name: "testSetString",
			args: args{
				tag:   "string",
				value: "example string",
				taggedStruct: &testStruct{
					Int:    0,
					Bool:   false,
					String: "example string",
				},
			},
			wantErr: false,
		},
		{
			name: "testInvalidKey",
			args: args{
				tag:   "not a key",
				value: "",
				taggedStruct: &testStruct{
					Int:    0,
					Bool:   false,
					String: "",
				},
			},
			wantErr: true,
		},
		{
			name: "testInvalidInt",
			args: args{
				tag:   "int",
				value: "false",
				taggedStruct: &testStruct{
					Int:    0,
					Bool:   false,
					String: "",
				},
			},
			wantErr: true,
		},
		{
			name: "testInvalidBool",
			args: args{
				tag:   "bool",
				value: "cheese",
				taggedStruct: &testStruct{
					Int:    0,
					Bool:   false,
					String: "",
				},
			},
			wantErr: true,
		},
		{
			name: "testMissingType",
			args: args{
				tag:   "float",
				value: "0.54",
				taggedStruct: &testStruct{
					Int:    0,
					Bool:   false,
					String: "",
				},
			},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := SetStructFieldByTag(tt.args.tag, tt.args.value, tt.args.taggedStruct); (err != nil) != tt.wantErr {
				t.Errorf("SetStructFieldByTag() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}
