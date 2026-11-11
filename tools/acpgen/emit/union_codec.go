package emit

import (
	"fmt"
	"strings"

	"github.com/gfx-labs/acpharness/tools/acpgen/ir"
	"github.com/gfx-labs/acpharness/tools/acpgen/load"
	"github.com/gfx-labs/acpharness/tools/acpgen/util"
)

// The union codec is written for encoding/json/v2. Upstream wrote it for v1:
// it decoded every union into a map of raw members to find the variant, and
// encoded a variant by marshalling it, unmarshalling the bytes into a
// map[string]any, setting the discriminator and marshalling the map.
// tools/acpgen/README.md says what that changed on the wire.

const (
	jsonPkg     = "encoding/json/v2"
	jsontextPkg = "encoding/json/jsontext"
)

// useJSONv2 names the v2 package json in f. Jennifer would otherwise alias it
// from the last element of its path.
func useJSONv2(f *File) {
	f.ImportName(jsonPkg, "json")
	f.ImportName(jsontextPkg, "jsontext")
}

// variantInfo is one variant of a union as emitUnion sees it.
type variantInfo struct {
	fieldName         string
	typeName          string
	required          []string
	isObject          bool
	isArray           bool
	arrayItemRequired []string
	discValue         string
	constPairs        [][2]string
	isNull            bool
	description       string
}

// jDecodeVariant emits: decode b into a new variant value and keep it.
func jDecodeVariant(union string, vi variantInfo) []Code {
	return []Code{
		Var().Id("v").Id(vi.typeName),
		If(List(Id("err")).Op(":=").Qual(jsonPkg, "Unmarshal").Call(Id("b"), Op("&").Id("v"), Id("opts")), Id("err").Op("!=").Nil()).Block(
			Return(Qual("fmt", "Errorf").Call(Lit(union+": variant "+vi.fieldName+": %w"), Id("err"))),
		),
		Id("u").Dot(vi.fieldName).Op("=").Op("&").Id("v"),
		Return(Nil()),
	}
}

// emitUnionUnmarshal emits UnmarshalJSONFrom. The variant is chosen in the
// order upstream chose it: a null variant for null, the discriminator, the
// first variant whose required members are all present, the first array
// variant whose items all have their required members, and last the first
// variant the value decodes into.
func emitUnionUnmarshal(f *File, name, discKey string, variants []variantInfo) {
	f.Func().Params(Id("u").Op("*").Id(name)).Id("UnmarshalJSONFrom").Params(Id("dec").Op("*").Qual(jsontextPkg, "Decoder")).Error().BlockFunc(func(g *Group) {
		g.List(Id("b"), Id("err")).Op(":=").Id("dec").Dot("ReadValue").Call()
		g.If(Id("err").Op("!=").Nil()).Block(Return(Id("err")))
		// The caller's options, such as RejectUnknownMembers, apply to the variant.
		g.Id("opts").Op(":=").Id("dec").Dot("Options").Call()

		for _, vi := range variants {
			if vi.isNull {
				g.If(Id("b").Dot("Kind").Call().Op("==").LitRune('n')).Block(
					Var().Id("v").Id(vi.typeName),
					Id("u").Dot(vi.fieldName).Op("=").Op("&").Id("v"),
					Return(Nil()),
				)
				break
			}
		}

		hasDisc := false
		if discKey != "" {
			for _, vi := range variants {
				hasDisc = hasDisc || vi.discValue != ""
			}
		}
		if hasDisc {
			// A discriminator that is absent or not a string selects no
			// variant here.
			g.List(Id("disc"), Id("_")).Op(":=").Id("stringMember").Call(Id("b"), Lit(discKey))
			g.Switch(Id("disc")).BlockFunc(func(sw *Group) {
				for _, vi := range variants {
					if vi.discValue != "" {
						sw.Case(Lit(vi.discValue)).Block(jDecodeVariant(name, vi)...)
					}
				}
			})
		}

		needNames := false
		for _, vi := range variants {
			needNames = needNames || (vi.isObject && len(vi.required) > 0)
		}
		if needNames {
			g.If(List(Id("names"), Id("ok")).Op(":=").Id("objectMembers").Call(Id("b")), Id("ok")).BlockFunc(func(obj *Group) {
				for _, vi := range variants {
					if !vi.isObject || len(vi.required) == 0 {
						continue
					}
					cond := Id("names").Dot("has").Call(Lit(vi.required[0]))
					for _, rk := range vi.required[1:] {
						cond = cond.Op("&&").Id("names").Dot("has").Call(Lit(rk))
					}
					obj.If(cond).Block(jDecodeVariant(name, vi)...)
				}
			})
		}

		needItems := false
		for _, vi := range variants {
			needItems = needItems || (vi.isArray && len(vi.arrayItemRequired) > 0)
		}
		if needItems {
			g.If(List(Id("items"), Id("ok")).Op(":=").Id("arrayItemMembers").Call(Id("b")), Id("ok")).BlockFunc(func(arr *Group) {
				for _, vi := range variants {
					if !vi.isArray || len(vi.arrayItemRequired) == 0 {
						continue
					}
					args := []Code{}
					for _, rk := range vi.arrayItemRequired {
						args = append(args, Lit(rk))
					}
					arr.If(Id("items").Dot("allHave").Call(args...)).Block(jDecodeVariant(name, vi)...)
				}
			})
		}

		for _, vi := range variants {
			g.Block(
				Var().Id("v").Id(vi.typeName),
				If(Qual(jsonPkg, "Unmarshal").Call(Id("b"), Op("&").Id("v"), Id("opts")).Op("==").Nil()).Block(
					Id("u").Dot(vi.fieldName).Op("=").Op("&").Id("v"),
					Return(Nil()),
				),
			)
		}
		g.Return(Qual("errors", "New").Call(Lit(name + ": no matching variant")))
	})
	f.Line()
}

// emitUnionMarshal emits MarshalJSONTo. The variant that is set is written
// as itself, with its const members set to their values. A const member is
// a field of the variant's struct, so no map is needed to add it.
func emitUnionMarshal(f *File, name string, variants []variantInfo) {
	f.Func().Params(Id("u").Id(name)).Id("MarshalJSONTo").Params(Id("enc").Op("*").Qual(jsontextPkg, "Encoder")).Error().BlockFunc(func(g *Group) {
		for _, vi := range variants {
			g.If(Id("u").Dot(vi.fieldName).Op("!=").Nil()).BlockFunc(func(gg *Group) {
				switch {
				case vi.isNull:
					gg.Return(Id("enc").Dot("WriteToken").Call(Qual(jsontextPkg, "Null")))
				case !vi.isObject || len(vi.constPairs) == 0:
					gg.Return(Qual(jsonPkg, "MarshalEncode").Call(Id("enc"), Id("u").Dot(vi.fieldName)))
				default:
					gg.Id("v").Op(":=").Op("*").Id("u").Dot(vi.fieldName)
					for _, kv := range vi.constPairs {
						gg.Id("v").Dot(util.ToExportedField(kv[0])).Op("=").Lit(kv[1])
					}
					gg.Return(Qual(jsonPkg, "MarshalEncode").Call(Id("enc"), Op("&").Id("v")))
				}
			})
		}
		g.Return(Qual("errors", "New").Call(Lit(name + ": no variant is set")))
	})
}

// omitOption returns the tag option that leaves an optional field of type t
// out of the output exactly when encoding/json v1 left it out with
// omitempty: a false, a zero number, an empty string, a nil pointer, a nil
// interface, and a slice or map that is nil or empty.
//
// v2's omitempty looks at the encoded value instead, so it would keep a
// false and drop a pointer to "" or an interface holding {}. Those take
// omitzero. A slice or map keeps omitempty, which drops both nil and empty.
// v1 never left a struct out, so a struct takes no option.
func omitOption(schema *load.Schema, t Code) string {
	const zero, empty, never = ",omitzero", ",omitempty", ""
	src := fmt.Sprintf("%#v", t)
	switch {
	case strings.HasPrefix(src, "*"):
		return zero
	case strings.HasPrefix(src, "[]"), strings.HasPrefix(src, "map["):
		return empty
	}
	switch src {
	case "string", "int", "float64", "bool", "any", "interface{}":
		return zero
	}
	def := schema.Defs[src]
	switch {
	case def == nil:
		// A struct generated for an inline object.
		return never
	case len(def.Enum) > 0, isStringConstUnion(def), isOpenStringEnum(def):
		return zero
	case len(def.AnyOf) > 0, len(def.OneOf) > 0:
		return never
	}
	switch ir.PrimaryType(def) {
	case "string", "integer", "number", "boolean":
		return zero
	case "array":
		return empty
	case "object":
		return never
	}
	// Emitted as "type X any".
	return zero
}

// defaultCodec is one property with a schema default.
type defaultCodec struct {
	fieldName   string
	propName    string
	defaultJSON string
	// allowNull: the schema lets the member be null, so only a missing
	// member takes the default.
	allowNull bool
	// nilable: the field is a slice or a map and the default is an array or
	// an object.
	nilable bool
}

// zeroDefault reports whether decoding the default into a zero field leaves
// it zero, so that applying it is not needed.
func zeroDefault(defaultJSON string) bool {
	switch defaultJSON {
	case "null", "false", "0", `""`:
		return true
	}
	return false
}

// emitDefaultsCodec emits UnmarshalJSONFrom for a struct with schema
// defaults: a member that is missing, or null where the schema does not
// allow null, takes its default.
//
// No MarshalJSON is emitted. Upstream emitted one to write a nil slice as []
// and a nil map as {}, which is what encoding/json/v2 writes anyway. A
// default it could not write that way stops the generator.
func emitDefaultsCodec(f *File, name string, defaults []defaultCodec) {
	applied := []defaultCodec{}
	for _, d := range defaults {
		if d.nilable && d.defaultJSON != "[]" && d.defaultJSON != "{}" {
			panic(fmt.Sprintf("%s.%s: default %s of a slice or map is not what a nil one encodes as", name, d.propName, d.defaultJSON))
		}
		if !zeroDefault(d.defaultJSON) {
			applied = append(applied, d)
		}
	}
	if len(applied) == 0 {
		return
	}
	f.Func().Params(Id("v").Op("*").Id(name)).Id("UnmarshalJSONFrom").Params(Id("dec").Op("*").Qual(jsontextPkg, "Decoder")).Error().BlockFunc(func(g *Group) {
		g.List(Id("b"), Id("err")).Op(":=").Id("dec").Dot("ReadValue").Call()
		g.If(Id("err").Op("!=").Nil()).Block(Return(Id("err")))
		// Alias has no methods, so this decode does not come back here.
		g.Type().Id("Alias").Id(name)
		g.Var().Id("a").Id("Alias")
		g.If(List(Id("err")).Op(":=").Qual(jsonPkg, "Unmarshal").Call(Id("b"), Op("&").Id("a"), Id("dec").Dot("Options").Call()), Id("err").Op("!=").Nil()).Block(Return(Id("err")))
		g.List(Id("members"), Id("_")).Op(":=").Id("objectMembers").Call(Id("b"))
		for _, d := range applied {
			cond := Op("!").Id("members").Dot("has").Call(Lit(d.propName))
			if !d.allowNull {
				cond = Id("members").Dot("missingOrNull").Call(Lit(d.propName))
			}
			g.If(cond).Block(
				If(List(Id("err")).Op(":=").Qual(jsonPkg, "Unmarshal").Call(Index().Byte().Parens(Lit(d.defaultJSON)), Op("&").Id("a").Dot(d.fieldName)), Id("err").Op("!=").Nil()).Block(
					Return(Qual("fmt", "Errorf").Call(Lit(name+"."+d.propName+": default: %w"), Id("err"))),
				),
			)
		}
		g.Op("*").Id("v").Op("=").Id(name).Call(Id("a"))
		g.Return(Nil())
	})
	f.Line()
}
