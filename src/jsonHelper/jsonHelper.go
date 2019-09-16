/**
 Package jsonHelper helps you set individual fields in structs based on its tag
 */
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