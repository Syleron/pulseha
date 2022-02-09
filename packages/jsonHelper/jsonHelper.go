// PulseHA - HA Cluster Daemon
// Copyright (C) 2017-2021  Andrew Zak <andrew@linux.com>
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published
// by the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU Affero General Public License for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with this program.  If not, see <http://www.gnu.org/licenses/>.

package jsonHelper

import (
	"errors"
	"fmt"
	"reflect"
	"strconv"
)

var (
 ErrInvalidConfigValue = errors.New("invalid config value")
 ErrInvalidConfigKey = errors.New("invalid config key")
)

/**
 SetStructFieldByTag takes a struct and sets the field that matches the supplied json tag to the value
 */
func SetStructFieldByTag(tag string, value string, taggedStruct interface{}) error {
	v := reflect.ValueOf(taggedStruct).Elem()
	fieldType := reflect.TypeOf(taggedStruct).Elem()

	for i := 0; i < v.NumField(); i++ {
		field := fieldType.Field(i)
		if field.Tag.Get("json") == tag {
			err := setField(v.Field(i), value)
			if err != nil {
				return ErrInvalidConfigValue
			}
			return nil
		}
	}
	return ErrInvalidConfigKey
}

/**
 setField finds the type of the field then converts the string value to the matching type
 */
func setField(field reflect.Value, value string) error {
	switch field.Kind() {
	case reflect.String:
		field.SetString(value)
	case reflect.Bool:
		b, err := strconv.ParseBool(value)
		if err != nil {
			return err
		}
		field.SetBool(b)
	case reflect.Int:
		i, err := strconv.ParseInt(value, 10, 64)
		if err != nil {
			return err
		}
		field.SetInt(i)

	default:
		return fmt.Errorf("unable to set field of type %s", field.Kind())
	}
	return nil
}