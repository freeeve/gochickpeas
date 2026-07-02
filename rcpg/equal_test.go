package rcpg_test

import (
	"math"

	"github.com/freeeve/gochickpeas/rcpg"
)

// graphsEqual is structural equality with f64 compared by bit pattern, so
// NaN payloads and -0.0 in the corpus compare exactly.
func graphsEqual(a, b *rcpg.GraphSection) bool {
	return a.NNodes == b.NNodes &&
		a.NRels == b.NRels &&
		u32sEqual(a.OutOffsets, b.OutOffsets) &&
		u32sEqual(a.OutNbrs, b.OutNbrs) &&
		u32sEqual(a.OutTypes, b.OutTypes) &&
		u32sEqual(a.InOffsets, b.InOffsets) &&
		u32sEqual(a.InNbrs, b.InNbrs) &&
		u32sEqual(a.InTypes, b.InTypes) &&
		bitmapIndexEqual(a.LabelIndex, b.LabelIndex) &&
		bitmapIndexEqual(a.TypeIndex, b.TypeIndex) &&
		columnsEqual(a.NodeColumns, b.NodeColumns) &&
		columnsEqual(a.RelColumns, b.RelColumns) &&
		versionEqual(a.Version, b.Version) &&
		stringsEqual(a.Atoms, b.Atoms)
}

func u32sEqual(a, b []uint32) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func stringsEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func versionEqual(a, b *string) bool {
	if (a == nil) != (b == nil) {
		return false
	}
	return a == nil || *a == *b
}

func bitmapIndexEqual(a, b []rcpg.BitmapEntry) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Atom != b[i].Atom || !a[i].Bitmap.Equals(b[i].Bitmap) {
			return false
		}
	}
	return true
}

func columnsEqual(a, b []rcpg.Column) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Key != b[i].Key || !columnDataEqual(a[i].Data, b[i].Data) {
			return false
		}
	}
	return true
}

func columnDataEqual(a, b rcpg.ColumnData) bool {
	switch x := a.(type) {
	case rcpg.DenseI64:
		y, ok := b.(rcpg.DenseI64)
		if !ok || len(x) != len(y) {
			return false
		}
		for i := range x {
			if x[i] != y[i] {
				return false
			}
		}
		return true
	case rcpg.DenseF64:
		y, ok := b.(rcpg.DenseF64)
		if !ok || len(x) != len(y) {
			return false
		}
		for i := range x {
			if math.Float64bits(x[i]) != math.Float64bits(y[i]) {
				return false
			}
		}
		return true
	case rcpg.DenseBool:
		y, ok := b.(rcpg.DenseBool)
		if !ok || x.Len != y.Len {
			return false
		}
		for i := uint32(0); i < x.Len; i++ {
			if x.Get(i) != y.Get(i) {
				return false
			}
		}
		return true
	case rcpg.DenseStr:
		y, ok := b.(rcpg.DenseStr)
		return ok && u32sEqual(x, y)
	case rcpg.SparseI64:
		y, ok := b.(rcpg.SparseI64)
		if !ok || len(x) != len(y) {
			return false
		}
		for i := range x {
			if x[i] != y[i] {
				return false
			}
		}
		return true
	case rcpg.SparseF64:
		y, ok := b.(rcpg.SparseF64)
		if !ok || len(x) != len(y) {
			return false
		}
		for i := range x {
			if x[i].ID != y[i].ID || math.Float64bits(x[i].Val) != math.Float64bits(y[i].Val) {
				return false
			}
		}
		return true
	case rcpg.SparseBool:
		y, ok := b.(rcpg.SparseBool)
		if !ok || len(x) != len(y) {
			return false
		}
		for i := range x {
			if x[i] != y[i] {
				return false
			}
		}
		return true
	case rcpg.SparseStr:
		y, ok := b.(rcpg.SparseStr)
		if !ok || len(x) != len(y) {
			return false
		}
		for i := range x {
			if x[i] != y[i] {
				return false
			}
		}
		return true
	}
	return false
}
