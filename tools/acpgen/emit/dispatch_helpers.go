// This file is modified from cmd/generate in github.com/coder/acp-go-sdk
// (commit 492745b, Apache-2.0). tools/acpgen/README.md lists the changes.

package emit

import (
	"github.com/gfx-labs/towa/tools/acpgen/ir"
)

// invInvalid: return invalid params with compact json-like message
func jInvInvalid() Code {
	return Return(Nil(), ID("NewInvalidParams").Call(Map(String()).Any().Values(Dict{Lit("error"): ID("err").Dot("Error").Call()})))
}

// retToReqErr: wrap error to JSON-RPC request error
func jRetToReqErr() Code { return Return(Nil(), ID("toReqErr").Call(ID("err"))) }

// jUnmarshalValidate emits var p T; json.Unmarshal; p.Validate
func jUnmarshalValidate(typeName string) []Code {
	return []Code{
		Var().Id("p").Id(typeName),
		If(List(ID("err")).Op(":=").Qual(jsonPkg, "Unmarshal").Call(ID("params"), Op("&").Id("p")), ID("err").Op("!=").Nil()).
			Block(jInvInvalid()),
		If(List(ID("err")).Op(":=").Id("p").Dot("Validate").Call(), ID("err").Op("!=").Nil()).
			Block(jInvInvalid()),
	}
}

// jAgentAssert returns prelude for interface assertions and the receiver name.
func jAgentAssert(binding ir.MethodBinding, methodName, paramType, respType string, hasResponse bool) ([]Code, string) {
	switch binding {
	case ir.BindAgentLoader:
		return []Code{
			List(ID("loader"), ID("ok")).Op(":=").Id("a").Dot("agent").Assert(ID("AgentLoader")),
			If(Op("!").Id("ok")).Block(Return(Nil(), ID("NewMethodNotFound").Call(ID("method")))),
		}, "loader"
	case ir.BindAgentExperimental:
		return jSingleMethodAssert(ID("a").Dot("agent"), "exp", methodName, paramType, respType, hasResponse)
	default:
		return nil, "a.agent"
	}
}

// jClientAssert returns prelude for interface assertions and the receiver name.
func jClientAssert(binding ir.MethodBinding, methodName, paramType, respType string, hasResponse bool) ([]Code, string) {
	switch binding {
	case ir.BindClientExperimental:
		return jSingleMethodAssert(ID("c").Dot("client"), "exp", methodName, paramType, respType, hasResponse)
	case ir.BindClientTerminal:
		return []Code{
			List(ID("t"), ID("ok")).Op(":=").Id("c").Dot("client").Assert(ID("ClientTerminal")),
			If(Op("!").Id("ok")).Block(Return(Nil(), ID("NewMethodNotFound").Call(ID("method")))),
		}, "t"
	default:
		return nil, "c.client"
	}
}

func jSingleMethodAssert(receiver Code, assertedName, methodName, paramType, respType string, hasResponse bool) ([]Code, string) {
	ifaceType := InterfaceFunc(func(g *Group) {
		method := g.Id(methodName).Params(Qual("context", "Context"), ID(paramType))
		if hasResponse {
			method.Params(ID(respType), Error())
		} else {
			method.Error()
		}
	})
	return []Code{
		List(ID(assertedName), ID("ok")).Op(":=").Add(receiver).Assert(ifaceType),
		If(Op("!").Id("ok")).Block(Return(Nil(), ID("NewMethodNotFound").Call(ID("method")))),
	}, assertedName
}

// Request call emitters for handlers
func jCallRequestNoResp(recv, methodName string) []Code {
	return []Code{
		If(List(ID("err")).Op(":=").Id(recv).Dot(methodName).Call(ID("ctx"), ID("p")), ID("err").Op("!=").Nil()).Block(jRetToReqErr()),
		Return(Nil(), Nil()),
	}
}

func jCallRequestWithResp(recv, methodName string) []Code {
	return []Code{
		List(ID("resp"), ID("err")).Op(":=").Id(recv).Dot(methodName).Call(ID("ctx"), ID("p")),
		If(ID("err").Op("!=").Nil()).Block(jRetToReqErr()),
		Return(ID("resp"), Nil()),
	}
}

func jCallNotification(recv, methodName string) []Code {
	return []Code{
		If(List(ID("err")).Op(":=").Id(recv).Dot(methodName).Call(ID("ctx"), ID("p")), ID("err").Op("!=").Nil()).Block(jRetToReqErr()),
		Return(Nil(), Nil()),
	}
}
