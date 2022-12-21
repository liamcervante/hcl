package typeexpr

import (
	"errors"
	"sort"
	"strconv"

	"github.com/zclconf/go-cty/cty"
	"github.com/zclconf/go-cty/cty/convert"
)

// Defaults represents a type tree which may contain default values for
// optional object attributes at any level. This is used to apply nested
// defaults to a given cty.Value.
type Defaults struct {
	// Type of the node for which these defaults apply. This is necessary in
	// order to determine how to inspect the Defaults and Children collections.
	Type cty.Type

	// DefaultValues contains the default values for each object attribute,
	// indexed by attribute name.
	DefaultValues map[string]cty.Value

	// Children is a map of Defaults for elements contained in this type. This
	// only applies to structural and collection types.
	//
	// The map is indexed by string instead of cty.Value because cty.Number
	// instances are non-comparable, due to embedding a *big.Float.
	//
	// Collections have a single element type, which is stored at key "".
	Children map[string]*Defaults
}

// Apply walks the given value, applying specified defaults wherever optional
// attributes are missing. The input and output values may have different
// types, to avoid this the input value should be converted into the desired
// type first.
//
// This function is permissive and does not report errors, assuming that the
// caller will have better context to report useful type conversion failure
// diagnostics.
func (d *Defaults) Apply(val cty.Value) cty.Value {
	applied, err := d.apply(val, false)
	if err != nil {
		// This shouldn't really happen, as the conversion is safe when the
		// concrete variable is false.
		panic(err)
	}
	return applied
}

func (d *Defaults) ApplyAndConvert(val cty.Value) (cty.Value, error) {
	return d.apply(val, true)
}

func (d *Defaults) apply(v cty.Value, concrete bool) (cty.Value, error) {
	// We don't apply defaults to null values or unknown values. To be clear,
	// we will overwrite children values with defaults if they are null but not
	// if the actual value is null.
	if !v.IsKnown() || v.IsNull() {
		return v, nil
	}

	// Also, do nothing if we have no defaults to apply.
	if len(d.DefaultValues) == 0 && len(d.Children) == 0 {
		return v, nil
	}

	v, marks := v.Unmark()

	switch {
	case v.Type().IsSetType(), v.Type().IsListType(), v.Type().IsTupleType():
		values, err := d.applyAsSlice(v, concrete)
		if err != nil {
			return cty.NilVal, err
		}

		if concrete {
			v, err = convert.Convert(cty.TupleVal(values), d.Type)
			if err != nil {
				return v, errors.New(convert.MismatchMessage(v.Type(), d.Type))
			}
		} else {
			v = d.unifyFromSlice(v.Type(), values)
		}
	case v.Type().IsObjectType(), v.Type().IsMapType():
		values, err := d.applyAsMap(v, concrete)
		if err != nil {
			return cty.NilVal, err
		}

		for key, defaultValue := range d.DefaultValues {
			if value, ok := values[key]; !ok || value.IsNull() {
				if defaults, ok := d.Children[key]; ok {
					var err error
					if values[key], err = defaults.apply(defaultValue, concrete); err != nil {
						return cty.NilVal, err
					}
					continue
				}
				values[key] = defaultValue
			}
		}

		if concrete {
			v, err = convert.Convert(cty.ObjectVal(values), d.Type)
			if err != nil {
				return v, errors.New(convert.MismatchMessage(v.Type(), d.Type))
			}
		} else {
			v = d.unifyFromMap(v.Type(), values)
		}
	}

	return v.WithMarks(marks), nil
}

func (d *Defaults) applyAsSlice(value cty.Value, concrete bool) ([]cty.Value, error) {
	var elements []cty.Value
	for ix, element := range value.AsValueSlice() {
		if childDefaults := d.getChild(ix); childDefaults != nil {
			element, err := childDefaults.apply(element, concrete)
			if err != nil {
				return nil, err
			}
			elements = append(elements, element)
			continue
		}
		elements = append(elements, element)
	}
	return elements, nil
}

func (d *Defaults) applyAsMap(value cty.Value, concrete bool) (map[string]cty.Value, error) {
	elements := make(map[string]cty.Value)
	for key, element := range value.AsValueMap() {
		if childDefaults := d.getChild(key); childDefaults != nil {
			var err error
			if elements[key], err = childDefaults.apply(element, concrete); err != nil {
				return nil, err
			}
			continue
		}
		elements[key] = element
	}
	return elements, nil
}

func (d *Defaults) getChild(key interface{}) *Defaults {
	switch {
	case d.Type.IsMapType(), d.Type.IsSetType(), d.Type.IsListType():
		return d.Children[""]
	case d.Type.IsTupleType():
		return d.Children[strconv.Itoa(key.(int))]
	case d.Type.IsObjectType():
		return d.Children[key.(string)]
	default:
		return nil
	}
}

func (d *Defaults) unifyFromSlice(target cty.Type, values []cty.Value) cty.Value {
	if target.IsTupleType() {
		return cty.TupleVal(values)
	}

	var types []cty.Type
	for _, value := range values {
		types = append(types, value.Type())
	}
	unify, conversions := convert.UnifyUnsafe(types)
	if unify == cty.NilType {
		return cty.TupleVal(values)
	}

	var converts []cty.Value
	for ix := 0; ix < len(conversions); ix++ {
		if conversions[ix] == nil {
			converts = append(converts, values[ix])
			continue
		}

		converted, err := conversions[ix](values[ix])
		if err != nil {
			return cty.TupleVal(values)
		}
		converts = append(converts, converted)
	}

	if target.IsSetType() {
		return cty.SetVal(converts)
	} else {
		return cty.ListVal(converts)
	}
}

func (d *Defaults) unifyFromMap(target cty.Type, values map[string]cty.Value) cty.Value {
	if target.IsObjectType() {
		return cty.ObjectVal(values)
	}

	var keys []string
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	var types []cty.Type
	for _, key := range keys {
		types = append(types, values[key].Type())
	}
	unify, conversions := convert.UnifyUnsafe(types)
	if unify == cty.NilType {
		return cty.ObjectVal(values)
	}

	converts := make(map[string]cty.Value)
	for ix, key := range keys {
		if conversions[ix] == nil {
			converts[key] = values[key]
			continue
		}

		var err error
		if converts[key], err = conversions[ix](values[key]); err != nil {
			return cty.ObjectVal(values)
		}
	}
	return cty.MapVal(converts)
}
