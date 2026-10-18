package canvas

import (
	"cmp"
	"fmt"
	"slices"
)

// Fold returns the state after one committed mutation, given the result the
// store returned for it, so a client can follow committed changes without
// reading the store again. The result must carry the revision right after
// c's. Fold never modifies c, and returns ErrInvalid when the mutation and
// result cannot follow c, which a client treats as a lost update.
func Fold(c Canvas, mutation Mutation, result Result) (Canvas, error) {
	if result.Revision != c.Revision+1 {
		return Canvas{}, fmt.Errorf("%w: revision %d does not follow %d", ErrInvalid, result.Revision, c.Revision)
	}
	switch mutation.(type) {
	case SetDimensions, CreateObject, SetContent, SetTransform, DeleteObject:
	default:
		return Canvas{}, fmt.Errorf("%w: unsupported mutation %T", ErrInvalid, mutation)
	}
	if err := mutation.validate(); err != nil {
		return Canvas{}, err
	}
	if target := objectOf(mutation); target != "" && target != result.ObjectID {
		return Canvas{}, fmt.Errorf("%w: result names another object", ErrInvalid)
	}
	next := c
	next.Revision = result.Revision
	next.Objects = slices.Clone(c.Objects)
	switch m := mutation.(type) {
	case SetDimensions:
		next.Dimensions = m.Dimensions
	case CreateObject:
		if err := validateIdentifier("object", result.ObjectID); err != nil {
			return Canvas{}, err
		}
		if index(next.Objects, result.ObjectID) >= 0 {
			return Canvas{}, fmt.Errorf("%w: object already exists", ErrInvalid)
		}
		if err := freeZ(next.Objects, "", result.Z); err != nil {
			return Canvas{}, err
		}
		next.Objects = append(next.Objects, Object{ID: result.ObjectID, Kind: m.Kind, Content: m.Content, Transform: m.Transform, Z: result.Z})
	case SetContent:
		i := index(next.Objects, m.ObjectID)
		if i < 0 {
			return Canvas{}, fmt.Errorf("%w: unknown object", ErrInvalid)
		}
		next.Objects[i].Content = m.Content
	case SetTransform:
		i := index(next.Objects, m.ObjectID)
		if i < 0 {
			return Canvas{}, fmt.Errorf("%w: unknown object", ErrInvalid)
		}
		if err := freeZ(next.Objects, m.ObjectID, result.Z); err != nil {
			return Canvas{}, err
		}
		next.Objects[i].Transform, next.Objects[i].Z = m.Transform, result.Z
	case DeleteObject:
		i := index(next.Objects, m.ObjectID)
		if i < 0 {
			return Canvas{}, fmt.Errorf("%w: unknown object", ErrInvalid)
		}
		next.Objects = slices.Delete(next.Objects, i, i+1)
	}
	slices.SortFunc(next.Objects, func(a, b Object) int { return cmp.Compare(a.Z, b.Z) })
	return next, nil
}

// objectOf returns the existing object a mutation addresses, if any.
func objectOf(mutation Mutation) string {
	switch m := mutation.(type) {
	case SetContent:
		return m.ObjectID
	case SetTransform:
		return m.ObjectID
	case DeleteObject:
		return m.ObjectID
	}
	return ""
}

func index(objects []Object, id string) int {
	return slices.IndexFunc(objects, func(o Object) bool { return o.ID == id })
}

// freeZ checks that z is a valid order no other object than self holds.
func freeZ(objects []Object, self string, z int64) error {
	if z < 1 {
		return fmt.Errorf("%w: order must be positive", ErrInvalid)
	}
	if slices.ContainsFunc(objects, func(o Object) bool { return o.ID != self && o.Z == z }) {
		return fmt.Errorf("%w: order %d already used", ErrInvalid, z)
	}
	return nil
}
