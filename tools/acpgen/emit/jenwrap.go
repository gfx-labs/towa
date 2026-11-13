package emit

import jen "github.com/dave/jennifer/jen"

// Types of jennifer, aliased so that calls stay short without a dot import.
type (
	// Code is jen.Code.
	Code = jen.Code
	// Dict is jen.Dict.
	Dict = jen.Dict
	// Group is jen.Group.
	Group = jen.Group
	// File is jen.File.
	File = jen.File
)

// NewFile and the names below are jennifer's constructors.
var (
	NewFile       = jen.NewFile
	ID            = jen.Id
	Lit           = jen.Lit
	Line          = jen.Line
	Func          = jen.Func
	For           = jen.For
	Range         = jen.Range
	Return        = jen.Return
	Nil           = jen.Nil
	String        = jen.String
	Int           = jen.Int
	Float64       = jen.Float64
	Bool          = jen.Bool
	Any           = jen.Any
	Map           = jen.Map
	Index         = jen.Index
	Qual          = jen.Qual
	Error         = jen.Error
	Case          = jen.Case
	Default       = jen.Default
	Switch        = jen.Switch
	Var           = jen.Var
	If            = jen.If
	List          = jen.List
	Op            = jen.Op
	InterfaceFunc = jen.InterfaceFunc
	Comment       = jen.Comment
)
