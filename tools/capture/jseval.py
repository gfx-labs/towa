"""A strict evaluator for the subset of JavaScript that Claude Code's bundle
uses to build prompts, tool descriptions and tool schemas.

Nothing from the bundle is executed. The source is parsed with tree-sitter and
walked here. Whatever depends on a running session (settings, the model in use,
the terminal, the network) evaluates to Unknown, and any decision that would
have to be taken on an Unknown raises Unresolved. A text is produced only when
every branch on the way to it was decided by the bundle's own constants or by
the stated profile.
"""

import math
import re
import unicodedata

import regex

import tree_sitter_javascript as tsj
from tree_sitter import Language, Parser

import zodschema

PARSER = Parser(Language(tsj.language()))
# Bounds on one evaluation. They stop a loop that only session state would
# end. Reaching one fails the item, it never shortens a text.
MAX_STEPS = 4_000_000
MAX_CALL_DEPTH = 120


class Unresolved(Exception):
    pass


class Undefined:
    def __repr__(self):
        return "undefined"

    def __bool__(self):
        return False


UNDEF = Undefined()


class Unknown:
    """A value that only a running session has.

    origin identifies where it came from, so that two reads of the same thing
    get the same answer when a branch has to be chosen."""

    def __init__(self, why, origin=None):
        self.why = why
        self.origin = origin if origin is not None else ("anonymous", id(self))

    def __repr__(self):
        return f"Unknown({self.why})"


class TooManyVariants(Unresolved):
    pass


class JSThrow(Unresolved):
    """A `throw` in the evaluated code. Caught by the code's own try/catch,
    and otherwise a failure like any other."""

    def __init__(self, value):
        super().__init__("upstream code throws on this path")
        self.value = value


class Break(Exception):
    pass


class Continue(Exception):
    pass


SHORT = object()  # an optional chain that stopped at null or undefined


class Return(Exception):
    def __init__(self, value):
        self.value = value


class Thunk:
    """An object member whose value is computed on first read."""

    def __init__(self, fn):
        self.fn = fn
        self.done = False
        self.value = None

    def get(self):
        if not self.done:
            self.value = self.fn()
            self.done = True
        return self.value


class Getter:
    def __init__(self, fn):
        self.fn = fn


class JSObject:
    def __init__(self):
        self.props = {}

    def get(self, key):
        if key not in self.props:
            return UNDEF
        value = self.props[key]
        if isinstance(value, Thunk):
            return value.get()
        if isinstance(value, Getter):
            return value.fn(self)
        return value

    def keys(self):
        return list(self.props)


class RuntimeObject(JSObject):
    """An object of the runtime: the members not given here are unknown."""

    def __init__(self, label):
        super().__init__()
        self.label = label

    def get(self, key):
        if key in self.props:
            return super().get(key)
        return Unknown(f"{self.label}.{key}", origin=("global", self.label, key))


class JSSet:
    def __init__(self, items):
        self.items = list(dict.fromkeys(items))


def map_key(k):
    """A Map key. Strings and numbers compare by value, objects by identity."""
    if isinstance(k, Unknown):
        raise Unresolved(f"map key depends on {k.why}")
    if isinstance(k, (str, int, float, bool)) or k is None or k is UNDEF:
        return (type(k).__name__, k if k is not UNDEF else None)
    return ("object", id(k))


class JSMap:
    def __init__(self, pairs):
        # map_key(key) -> (key, value), so iteration gives the key back.
        self.items = {map_key(k): (k, v) for k, v in pairs}


class JSRegex:
    def __init__(self, source, flags):
        self.source = source
        self.flags = flags


class Function:
    def __init__(self, interp, node, env, module, this=UNDEF, name=""):
        self.interp, self.node, self.env, self.module = interp, node, env, module
        self.this = this
        self.name = name
        self.is_arrow = node.type == "arrow_function"

    def call(self, this, args):
        return self.interp.call_function(self, this, args)


class Builtin:
    def __init__(self, fn, name=""):
        self.fn = fn
        self.name = name

    def call(self, this, args):
        try:
            return self.fn(*args)
        except (TypeError, ValueError, AttributeError, IndexError, KeyError, OverflowError, ZeroDivisionError) as e:
            # An argument this stand-in for a runtime function does not model,
            # usually a session value. The item fails; nothing is substituted.
            unknown = next((a for a in args if isinstance(a, Unknown)), None)
            detail = f"given {unknown.why}" if unknown is not None else f"{type(e).__name__}: {e}"
            raise Unresolved(f"runtime function {self.name or getattr(self.fn, '__name__', '?')} {detail}") from e


class JSClass:
    def __init__(self, interp, node, env, module):
        self.interp, self.node, self.env, self.module = interp, node, env, module


class Env:
    def __init__(self, parent=None):
        self.vars = {}
        self.parent = parent

    def lookup(self, name):
        env = self
        while env is not None:
            if name in env.vars:
                return env
            env = env.parent
        return None


def text(node):
    return node.text.decode("utf-8")


SIMPLE_ESCAPES = {"n": "\n", "t": "\t", "r": "\r", "b": "\b", "f": "\f", "v": "\v", "0": "\0"}


def unescape(seq):
    body = seq[1:]
    if body[0] == "u":
        if body[1] == "{":
            return chr(int(body[2:-1], 16))
        return chr(int(body[1:5], 16))
    if body[0] == "x":
        return chr(int(body[1:3], 16))
    if body in ("\n", "\r\n"):
        return ""
    return SIMPLE_ESCAPES.get(body, body)


def join_surrogates(s):
    # JS strings are UTF-16: \ud83d\ude00 written as two escapes is one character.
    return s.encode("utf-16", "surrogatepass").decode("utf-16")


def truthy(v):
    if isinstance(v, Unknown):
        raise Unresolved(f"branch depends on {v.why}")
    if v is UNDEF or v is None or v is False:
        return False
    if isinstance(v, (int, float)) and not isinstance(v, bool):
        return not (v == 0 or (isinstance(v, float) and math.isnan(v)))
    if isinstance(v, str):
        return v != ""
    return True


def number_to_string(n):
    if isinstance(n, float):
        if math.isinf(n):
            return "Infinity" if n > 0 else "-Infinity"
        if math.isnan(n):
            return "NaN"
        if n == int(n) and abs(n) < 1e21:
            return str(int(n))
        return repr(n)
    return str(n)


def to_string(v):
    if isinstance(v, Unknown):
        raise Unresolved(f"text depends on {v.why}")
    if isinstance(v, str):
        return v
    if v is True:
        return "true"
    if v is False:
        return "false"
    if v is None:
        return "null"
    if v is UNDEF:
        return "undefined"
    if isinstance(v, (int, float)):
        return number_to_string(v)
    if isinstance(v, list):
        return ",".join("" if x is None or x is UNDEF else to_string(x) for x in v)
    raise Unresolved(f"cannot convert {type(v).__name__} to a string")


def strict_equal(a, b):
    if isinstance(a, Unknown) or isinstance(b, Unknown):
        raise Unresolved(f"comparison depends on {(a if isinstance(a, Unknown) else b).why}")
    if isinstance(a, bool) != isinstance(b, bool):
        return False
    if isinstance(a, (int, float)) and isinstance(b, (int, float)):
        return a == b
    if isinstance(a, str) and isinstance(b, str):
        return a == b
    return a is b


def to_python(v):
    """A JS value as plain JSON data. Fails on anything JSON cannot hold."""
    if isinstance(v, Unknown):
        raise Unresolved(f"value depends on {v.why}")
    if v is None or isinstance(v, (bool, str, int)):
        return v
    if isinstance(v, float):
        return int(v) if v == int(v) else v
    if isinstance(v, list):
        return [to_python(x) for x in v]
    if isinstance(v, JSObject):
        return {k: to_python(v.get(k)) for k in v.keys() if v.get(k) is not UNDEF}
    raise Unresolved(f"{type(v).__name__} is not JSON data")


class Module:
    def __init__(self, name, source):
        self.name = name
        self.source = source
        self.tree = PARSER.parse(source)
        if self.tree.root_node.has_error:
            raise Unresolved(f"{name}: tree-sitter reported a syntax error")
        self.decls = {}
        self.imports = {}
        self.exports = {}
        self.values = {}
        for node in self.tree.root_node.children:
            self.scan(node)

    def scan(self, node):
        t = node.type
        if t in ("function_declaration", "generator_function_declaration", "class_declaration"):
            self.decls[text(node.child_by_field_name("name"))] = ("decl", node)
        elif t in ("variable_declaration", "lexical_declaration"):
            for d in node.named_children:
                if d.type != "variable_declarator":
                    continue
                name = d.child_by_field_name("name")
                if name.type == "identifier":
                    self.decls[text(name)] = ("var", d.child_by_field_name("value"))
        elif t == "import_statement":
            source = node.child_by_field_name("source")
            path = "".join(text(c) for c in source.named_children)
            for clause in node.named_children:
                if clause.type != "import_clause":
                    continue
                for part in clause.named_children:
                    if part.type == "named_imports":
                        for spec in part.named_children:
                            name = text(spec.child_by_field_name("name"))
                            alias = spec.child_by_field_name("alias")
                            self.imports[text(alias) if alias else name] = (path, name)
                    elif part.type == "namespace_import":
                        self.imports[text(part.named_children[0])] = (path, "*")
                    elif part.type == "identifier":
                        self.imports[text(part)] = (path, "default")
        elif t == "export_statement":
            for clause in node.named_children:
                if clause.type == "export_clause":
                    for spec in clause.named_children:
                        name = text(spec.child_by_field_name("name"))
                        alias = spec.child_by_field_name("alias")
                        self.exports[text(alias) if alias else name] = name
                elif clause.type in ("function_declaration", "class_declaration", "lexical_declaration", "variable_declaration"):
                    self.scan(clause)


class Interp:
    def __init__(self, sources, platform):
        """sources: module path -> bytes. platform: value of process.platform."""
        self.sources = sources
        self.modules = {}
        self.platform = platform
        self.steps = 0
        self.stack = []
        # Fork state. A condition on an Unknown is decided by `plan` when one
        # is being replayed, else taken as false, and recorded in `trace` so
        # the caller can run again with the other answer.
        self.forking = False
        self.plan = []
        self.trace = []
        self.decided = {}
        self.flags = {}
        self.env_vars = set()
        self.zod = zodschema.Zod()
        self.zod_modules = {}

    # -- forks -----------------------------------------------------------

    def decide(self, node, module, unknown, question):
        if not self.forking:
            raise Unresolved(f"{question} depends on {unknown.why}")
        # A comparison is a different question for each thing compared with.
        key = (unknown.origin, question, text(node) if question == "is true" else "")
        if key in self.decided:
            return self.decided[key]
        index = len(self.trace)
        value = self.plan[index] if index < len(self.plan) else False
        self.decided[key] = value
        self.trace.append(
            {"module": module.name, "offset": node.start_byte, "question": question, "source": text(node), "depends_on": unknown.why, "answer": value}
        )
        return value

    def truth(self, value, node, module):
        if isinstance(value, Unknown):
            return self.decide(node, module, value, "is truthy")
        return truthy(value)

    def variants(self, compute, limit):
        """Run compute() once per combination of answers to the conditions
        that could not be decided. Yields (trace, result or Unresolved)."""
        plan = []
        count = 0
        while True:
            count += 1
            if count > limit:
                raise TooManyVariants(f"more than {limit} combinations of undecided conditions")
            for module in self.modules.values():
                module.values = {}
            self.forking, self.plan, self.trace, self.decided, self.steps = True, plan, [], {}, 0
            try:
                result = compute()
            except Unresolved as e:
                result = e
            finally:
                self.forking = False
            trace = self.trace
            yield trace, result
            answers = [d["answer"] for d in trace]
            while answers and answers[-1]:
                answers.pop()
            if not answers:
                return
            plan = answers[:-1] + [True]

    # -- modules ---------------------------------------------------------

    def module(self, name):
        if name not in self.modules:
            if name not in self.sources:
                raise Unresolved(f"module {name} is not in the bundle")
            self.modules[name] = Module(name, self.sources[name])
        return self.modules[name]

    def module_value(self, module, name, seen=()):
        if name in module.values:
            return module.values[name]
        if name in module.decls:
            kind, node = module.decls[name]
            if (module.name, name) in seen:
                raise Unresolved(f"cyclic initialiser for {name}")
            builder = self.zod_builder(module, name, node) if kind == "decl" else None
            if builder is not None:
                value = builder
            elif kind == "decl":
                if node.type == "class_declaration":
                    value = JSClass(self, node, None, module)
                else:
                    value = Function(self, node, None, module, name=name)
            elif node is None:
                value = Unknown(f"{name}, a module variable that is assigned at run time", origin=("module", module.name, name))
            else:
                module.values[name] = Unknown(f"{name} while it is being initialised")
                forking, self.forking = self.forking, False
                try:
                    value = self.eval(node, Env(), module)
                except Unresolved as e:
                    value = Unknown(f"{name} ({e})", origin=("module", module.name, name))
                finally:
                    self.forking = forking
            module.values[name] = value
            return value
        if name in module.imports:
            path, exported = module.imports[name]
            if not path.startswith("/$bunfs/"):
                value = Unknown(f"runtime module {path}", origin=("import", path, exported))
            elif exported in ("*", "default"):
                value = Unknown(f"namespace import of {path}", origin=("import", path, exported))
            else:
                target = self.module(path)
                if exported not in target.exports:
                    raise Unresolved(f"{path} does not export {exported}")
                value = self.module_value(target, target.exports[exported], seen + ((module.name, name),))
            module.values[name] = value
            return value
        return None

    def zod_builder(self, module, name, node):
        """A function of the zod module that constructs a schema, as a builtin."""
        if module.name not in self.zod_modules:
            self.zod_modules[module.name] = zodschema.classify_module(module.source.decode("utf-8"))
        kind = self.zod_modules[module.name].get(name)
        if kind is None:
            return None
        return Builtin(lambda *args: self.zod.build(kind, args, self), name=f"zod:{kind}")

    # -- calls -----------------------------------------------------------

    def bind_pattern(self, pattern, value, env, module):
        t = pattern.type
        if isinstance(value, Unknown) and t in ("object_pattern", "array_pattern"):
            for name in pattern_names(pattern):
                env.vars[name] = Unknown(f"{name}, destructured from {value.why}", origin=("member", value.origin, name))
            return
        if t == "identifier":
            env.vars[text(pattern)] = value
        elif t == "assignment_pattern":
            if value is UNDEF:
                value = self.eval(pattern.child_by_field_name("right"), env, module)
            self.bind_pattern(pattern.child_by_field_name("left"), value, env, module)
        elif t == "object_pattern":
            used = []
            for part in pattern.named_children:
                if part.type == "shorthand_property_identifier_pattern":
                    key = text(part)
                    used.append(key)
                    env.vars[key] = self.get_member(value, key)
                elif part.type == "pair_pattern":
                    key_node = part.child_by_field_name("key")
                    key = self.property_key(key_node, env, module)
                    used.append(key)
                    self.bind_pattern(part.child_by_field_name("value"), self.get_member(value, key), env, module)
                elif part.type == "object_assignment_pattern":
                    left = part.child_by_field_name("left")
                    key = text(left)
                    used.append(key)
                    got = self.get_member(value, key)
                    if got is UNDEF:
                        got = self.eval(part.child_by_field_name("right"), env, module)
                    env.vars[key] = got
                elif part.type == "rest_pattern":
                    if not isinstance(value, JSObject):
                        raise Unresolved("object rest of a value that is not a known object")
                    rest = JSObject()
                    for k in value.keys():
                        if k not in used:
                            rest.props[k] = value.props[k]
                    env.vars[text(part.named_children[0])] = rest
                else:
                    raise Unresolved(f"unsupported pattern {part.type}")
        elif t == "array_pattern":
            if not isinstance(value, list):
                raise Unresolved("array pattern on a value that is not a known array")
            for i, part in enumerate(pattern.named_children):
                if part.type == "rest_pattern":
                    env.vars[text(part.named_children[0])] = value[i:]
                else:
                    self.bind_pattern(part, value[i] if i < len(value) else UNDEF, env, module)
        else:
            raise Unresolved(f"unsupported pattern {t}")

    def call_function(self, fn, this, args):
        if len(self.stack) >= MAX_CALL_DEPTH:
            raise Unresolved(f"call depth over {MAX_CALL_DEPTH}: recursion that only run-time state ends")
        self.stack.append(fn.name or "<anonymous>")
        try:
            return self.run_function(fn, this, args)
        except TooManyVariants:
            raise
        except Unresolved as e:
            # Keep the innermost call path for the manifest's reason text.
            if not getattr(e, "path", None):
                e.path = list(self.stack)
            if not self.forking:
                raise
            # The call needs a session. Its result is unknown, not guessed:
            # a branch on it is written out both ways with this reason, and
            # a text that contains it fails.
            constants = tuple(repr(a) if isinstance(a, (str, int, float, bool)) or a is None else type(a).__name__ for a in args)
            return Unknown(
                f"the result of {fn.name or 'a function'}(), which cannot be computed without a session ({e})",
                origin=("failed call", fn.module.name, fn.node.start_byte, constants),
            )
        finally:
            self.stack.pop()

    def run_function(self, fn, this, args):
        node = fn.node
        if node.type in ("generator_function_declaration", "generator_function"):
            raise Unresolved("generator function")
        env = Env(fn.env)
        if not fn.is_arrow:
            env.vars["this"] = this
        elif fn.this is not UNDEF:
            env.vars["this"] = fn.this
        params = node.child_by_field_name("parameters")
        if params is None:
            single = node.child_by_field_name("parameter")
            if single is not None:
                env.vars[text(single)] = args[0] if args else UNDEF
        else:
            for i, p in enumerate(params.named_children):
                if p.type == "rest_pattern":
                    env.vars[text(p.named_children[0])] = list(args[i:])
                else:
                    self.bind_pattern(p, args[i] if i < len(args) else UNDEF, env, fn.module)
        body = node.child_by_field_name("body")
        if body.type != "statement_block":
            return self.eval(body, env, fn.module)
        try:
            self.exec_block(body, env, fn.module)
        except Return as r:
            return r.value
        return UNDEF

    def call(self, callee, this, args, what):
        if isinstance(callee, Unknown):
            constants = tuple(repr(a) if isinstance(a, (str, int, float, bool)) or a is None else type(a).__name__ for a in args)
            return Unknown(f"the result of {what}", origin=("call", callee.origin, constants))
        if isinstance(callee, (Function, Builtin)):
            return callee.call(this, args)
        if callee is UNDEF:
            raise Unresolved(f"{what} is not defined")
        raise Unresolved(f"{what} is not callable here ({type(callee).__name__})")

    # -- statements ------------------------------------------------------

    def exec_block(self, block, env, module):
        for s in block.named_children:
            if s.type == "function_declaration":
                name = text(s.child_by_field_name("name"))
                env.vars[name] = Function(self, s, env, module, name=name)
        for s in block.named_children:
            self.exec(s, env, module)

    def exec(self, s, env, module):
        t = s.type
        if t in ("lexical_declaration", "variable_declaration"):
            for d in s.named_children:
                value_node = d.child_by_field_name("value")
                value = self.eval(value_node, env, module) if value_node is not None else UNDEF
                self.bind_pattern(d.child_by_field_name("name"), value, env, module)
        elif t == "return_statement":
            raise Return(self.eval(s.named_children[0], env, module) if s.named_children else UNDEF)
        elif t == "if_statement":
            condition = s.child_by_field_name("condition")
            if self.truth(self.eval(condition, env, module), condition, module):
                self.exec(s.child_by_field_name("consequence"), env, module)
            else:
                alt = s.child_by_field_name("alternative")
                if alt is not None:
                    self.exec(alt.named_children[0], env, module)
        elif t == "statement_block":
            self.exec_block(s, Env(env), module)
        elif t == "expression_statement":
            self.eval(s.named_children[0], env, module)
        elif t == "for_in_statement":
            if not any(c.type == "of" for c in s.children):
                raise Unresolved("for-in loop")
            items = self.eval(s.child_by_field_name("right"), env, module)
            for item in self.iterate(items):
                inner = Env(env)
                self.bind_pattern(s.child_by_field_name("left"), item, inner, module)
                try:
                    self.exec(s.child_by_field_name("body"), inner, module)
                except Continue:
                    continue
                except Break:
                    break
        elif t in ("function_declaration", "empty_statement", "comment"):
            pass
        elif t == "throw_statement":
            raise JSThrow(self.eval(s.named_children[0], env, module))
        elif t == "break_statement":
            if s.named_children:
                raise Unresolved("labelled break")
            raise Break()
        elif t == "continue_statement":
            if s.named_children:
                raise Unresolved("labelled continue")
            raise Continue()
        elif t == "try_statement":
            self.exec_try(s, env, module)
        elif t == "switch_statement":
            self.exec_switch(s, env, module)
        else:
            raise Unresolved(f"unsupported statement {t}")

    def exec_try(self, s, env, module):
        handler = s.child_by_field_name("handler")
        finalizer = s.child_by_field_name("finalizer")
        try:
            try:
                self.exec_block(s.child_by_field_name("body"), Env(env), module)
            except JSThrow as thrown:
                if handler is None:
                    raise
                inner = Env(env)
                parameter = handler.child_by_field_name("parameter")
                if parameter is not None:
                    self.bind_pattern(parameter, thrown.value, inner, module)
                self.exec_block(handler.child_by_field_name("body"), inner, module)
        finally:
            if finalizer is not None:
                self.exec_block(finalizer.child_by_field_name("body"), Env(env), module)

    def exec_switch(self, s, env, module):
        value = self.eval(s.child_by_field_name("value"), env, module)
        if isinstance(value, Unknown):
            raise Unresolved(f"switch on {value.why}")
        cases = [c for c in s.child_by_field_name("body").named_children if c.type in ("switch_case", "switch_default")]
        start = None
        for index, case in enumerate(cases):
            if case.type == "switch_case" and strict_equal(value, self.eval(case.child_by_field_name("value"), env, module)):
                start = index
                break
        if start is None:
            start = next((i for i, c in enumerate(cases) if c.type == "switch_default"), None)
        if start is None:
            return
        inner = Env(env)
        try:
            for case in cases[start:]:
                for statement in case.children_by_field_name("body"):
                    self.exec(statement, inner, module)
        except Break:
            pass

    def iterate(self, v):
        if isinstance(v, list):
            return v
        if isinstance(v, JSSet):
            return v.items
        if isinstance(v, JSMap):
            return [[k, val] for k, val in v.items.values()]
        if isinstance(v, str):
            return list(v)
        if isinstance(v, Unknown):
            raise Unresolved(f"iteration over {v.why}")
        raise Unresolved(f"cannot iterate {type(v).__name__}")

    # -- expressions -----------------------------------------------------

    def property_key(self, node, env, module):
        if node.type in ("property_identifier", "identifier", "private_property_identifier"):
            return text(node)
        if node.type == "string":
            return self.eval(node, env, module)
        if node.type == "number":
            return to_string(self.eval(node, env, module))
        if node.type == "computed_property_name":
            return to_string(self.eval(node.named_children[0], env, module))
        raise Unresolved(f"unsupported property key {node.type}")

    def lookup(self, name, env, module):
        scope = env.lookup(name) if env is not None else None
        if scope is not None:
            return scope.vars[name]
        value = self.module_value(module, name)
        if value is not None:
            return value
        if name in GLOBALS:
            return GLOBALS[name]
        if name == "process":
            return self.process_object()
        return Unknown(f"the global {name}", origin=("global", name))

    def process_object(self):
        process = RuntimeObject("process")
        process.props["platform"] = self.platform
        return process

    def get_member(self, obj, key):
        if isinstance(obj, Unknown):
            # The bundle reads its environment through a wrapper object whose
            # definition is runtime state. A constant-case member of an unknown
            # object is taken to be an environment variable, and unset.
            if re.fullmatch(r"[A-Z][A-Z0-9_]*", key):
                self.env_vars.add(key)
                return UNDEF
            # Same object: its prototype carries the build's platform constant.
            if key == "platform":
                return self.platform
            return Unknown(f"{obj.why}.{key}", origin=("member", obj.origin, key))
        if obj is UNDEF or obj is None:
            raise Unresolved(f"property {key} of {obj!r}")
        if isinstance(obj, JSObject):
            return obj.get(key)
        if isinstance(obj, zodschema.Schema):
            return self.zod.member(obj, key, self)
        if isinstance(obj, str):
            return string_member(self, obj, key)
        if isinstance(obj, list):
            return array_member(self, obj, key)
        if isinstance(obj, JSSet):
            if key == "has":
                return Builtin(lambda v: any(strict_equal(v, x) for x in obj.items))
            if key == "size":
                return len(obj.items)
            if key == "add":

                def add(v):
                    if not any(strict_equal(v, x) for x in obj.items):
                        obj.items.append(v)
                    return obj

                return Builtin(add)
        if isinstance(obj, JSMap):
            if key == "size":
                return len(obj.items)
            if key == "get":
                return Builtin(lambda k: obj.items.get(map_key(k), (k, UNDEF))[1])
            if key == "has":
                return Builtin(lambda k: map_key(k) in obj.items)
            if key == "set":

                def put(k, v):
                    obj.items[map_key(k)] = (k, v)
                    return obj

                return Builtin(put)
        if isinstance(obj, JSRegex):
            if key == "source":
                return obj.source
        if isinstance(obj, (Function, Builtin)):
            return Unknown(f"property {key} of a function", origin=("function property", id(obj), key))
        if isinstance(obj, (int, float)) and key == "toString":
            return Builtin(lambda: number_to_string(obj))
        raise Unresolved(f"unsupported member {key} on {type(obj).__name__}")

    def eval(self, n, env, module):
        self.steps += 1
        if self.steps > MAX_STEPS:
            raise Unresolved("evaluation step limit reached")
        t = n.type
        if t == "identifier":
            return self.lookup(text(n), env, module)
        if t == "string":
            return join_surrogates(
                "".join(unescape(text(c)) if c.type == "escape_sequence" else text(c) for c in n.named_children)
            )
        if t == "template_string":
            out = []
            for c in n.named_children:
                if c.type == "escape_sequence":
                    out.append(unescape(text(c)))
                elif c.type == "template_substitution":
                    out.append(to_string(self.eval(c.named_children[0], env, module)))
                else:
                    out.append(text(c).replace("\r\n", "\n"))
            return join_surrogates("".join(out))
        if t == "number":
            raw = text(n).replace("_", "")
            if raw.lower().startswith(("0x", "0o", "0b")):
                return int(raw, 0)
            value = float(raw)
            return int(value) if value == int(value) and abs(value) < 2**63 else value
        if t == "true":
            return True
        if t == "false":
            return False
        if t == "null":
            return None
        if t == "undefined":
            return UNDEF
        if t == "this":
            scope = env.lookup("this")
            return scope.vars["this"] if scope else UNDEF
        if t == "parenthesized_expression":
            return self.eval(n.named_children[0], env, module)
        if t == "array":
            out = []
            for c in n.named_children:
                if c.type == "spread_element":
                    out.extend(self.iterate(self.eval(c.named_children[0], env, module)))
                else:
                    out.append(self.eval(c, env, module))
            return out
        if t == "object":
            return self.eval_object(n, env, module)
        if t in ("arrow_function", "function_expression"):
            scope = env.lookup("this")
            return Function(self, n, env, module, this=scope.vars["this"] if scope else UNDEF)
        if t in ("member_expression", "subscript_expression", "call_expression"):
            value = self.chain(n, env, module)
            return UNDEF if value is SHORT else value
        if t == "meta_property":
            meta = JSObject()
            meta.props["require"] = Builtin(self.namespace)
            return meta
        if t == "new_expression":
            return self.eval_new(n, env, module)
        if t == "ternary_expression":
            condition = n.child_by_field_name("condition")
            branch = "consequence" if self.truth(self.eval(condition, env, module), condition, module) else "alternative"
            return self.eval(n.child_by_field_name(branch), env, module)
        if t == "binary_expression":
            return self.eval_binary(n, env, module)
        if t == "unary_expression":
            op = text(n.child_by_field_name("operator"))
            if op == "typeof":
                arg = n.child_by_field_name("argument")
                value = self.eval(arg, env, module)
                return typeof(value)
            value = self.eval(n.child_by_field_name("argument"), env, module)
            if op == "!":
                return not self.truth(value, n.child_by_field_name("argument"), module)
            if op == "void":
                return UNDEF
            if isinstance(value, Unknown):
                raise Unresolved(f"arithmetic on {value.why}")
            if op == "-" and isinstance(value, (int, float)):
                return -value
            if op == "+" and isinstance(value, (int, float)):
                return value
            raise Unresolved(f"unsupported unary operator {op}")
        if t == "await_expression":
            return self.eval(n.named_children[0], env, module)
        if t == "sequence_expression":
            value = UNDEF
            for c in n.named_children:
                value = self.eval(c, env, module)
            return value
        if t == "assignment_expression":
            value = self.eval(n.child_by_field_name("right"), env, module)
            self.assign(n.child_by_field_name("left"), value, env, module)
            return value
        if t == "augmented_assignment_expression":
            op = text(n.child_by_field_name("operator"))
            left = n.child_by_field_name("left")
            current = self.eval(left, env, module)
            if op == "??=":
                if isinstance(current, Unknown):
                    if not self.decide(left, module, current, "is null or undefined"):
                        return current
                    current = UNDEF
                if current is UNDEF or current is None:
                    current = self.eval(n.child_by_field_name("right"), env, module)
                    self.assign(left, current, env, module)
                return current
            raise Unresolved(f"unsupported assignment operator {op}")
        if t == "regex":
            flags = n.child_by_field_name("flags")
            return JSRegex(text(n.child_by_field_name("pattern")), text(flags) if flags else "")
        if t == "spread_element":
            raise Unresolved("spread outside an array, object or call")
        if t == "class":
            return JSClass(self, n, env, module)
        raise Unresolved(f"unsupported expression {t}")

    def chain_object(self, node, env, module):
        """The object of a member access or call. SHORT when an optional
        chain to its left has already stopped: `a?.b.c` is undefined as a whole."""
        if node.type in ("member_expression", "subscript_expression", "call_expression"):
            return self.chain(node, env, module)
        return self.eval(node, env, module)

    def chain(self, n, env, module):
        t = n.type
        if t == "call_expression":
            return self.eval_call(n, env, module)
        obj = self.chain_object(n.child_by_field_name("object"), env, module)
        if obj is SHORT:
            return SHORT
        if (obj is UNDEF or obj is None) and any(c.type == "optional_chain" for c in n.children):
            return SHORT
        if t == "member_expression":
            return self.get_member(obj, text(n.child_by_field_name("property")))
        index = self.eval(n.child_by_field_name("index"), env, module)
        if isinstance(obj, Unknown):
            return Unknown(f"an element of {obj.why}", origin=("member", obj.origin, repr(index) if not isinstance(index, Unknown) else index.origin))
        if isinstance(index, Unknown):
            raise Unresolved(f"index depends on {index.why}")
        if isinstance(obj, list) and isinstance(index, (int, float)):
            return obj[int(index)] if 0 <= index < len(obj) else UNDEF
        return self.get_member(obj, to_string(index))

    def namespace(self, path):
        """The exports of an embedded module, for import() and import.meta.require()."""
        if not isinstance(path, str) or path not in self.sources:
            return Unknown(f"module {path!r}, which is not JavaScript embedded in the bundle", origin=("import", repr(path)))
        target = self.module(path)
        ns = JSObject()
        for exported, local in target.exports.items():
            ns.props[exported] = Thunk(lambda local=local: self.module_value(target, local))
        return ns

    def assign(self, target, value, env, module):
        if target.type == "identifier":
            name = text(target)
            scope = env.lookup(name)
            if scope is None:
                raise Unresolved(f"assignment to the module variable {name}")
            scope.vars[name] = value
        elif target.type == "member_expression":
            obj = self.eval(target.child_by_field_name("object"), env, module)
            if not isinstance(obj, JSObject):
                raise Unresolved("assignment to a member of a runtime value")
            obj.props[text(target.child_by_field_name("property"))] = value
        else:
            raise Unresolved(f"unsupported assignment target {target.type}")

    def eval_object(self, n, env, module):
        obj = JSObject()
        for c in n.named_children:
            if c.type == "pair":
                key = self.property_key(c.child_by_field_name("key"), env, module)
                value_node = c.child_by_field_name("value")
                obj.props[key] = Thunk(lambda v=value_node: self.eval(v, env, module))
            elif c.type == "shorthand_property_identifier":
                obj.props[text(c)] = Thunk(lambda v=c: self.lookup(text(v), env, module))
            elif c.type == "method_definition":
                key = self.property_key(c.child_by_field_name("name"), env, module)
                fn = Function(self, c, env, module, name=key)
                if any(x.type == "get" for x in c.children):
                    obj.props[key] = Getter(lambda this, f=fn: f.call(this, []))
                elif any(x.type == "set" for x in c.children):
                    continue
                else:
                    obj.props[key] = fn
            elif c.type == "spread_element":
                source = self.eval(c.named_children[0], env, module)
                if source is UNDEF or source is None or source is False:
                    continue
                if not isinstance(source, JSObject):
                    if isinstance(source, Unknown):
                        raise Unresolved(f"object spread of {source.why}")
                    raise Unresolved(f"object spread of {type(source).__name__}")
                for k in source.keys():
                    obj.props[k] = source.props[k] if not isinstance(source.props[k], Getter) else source.get(k)
            elif c.type == "comment":
                continue
            else:
                raise Unresolved(f"unsupported object member {c.type}")
        return obj

    def eval_args(self, node, env, module):
        args = []
        for c in node.named_children:
            if c.type == "spread_element":
                args.extend(self.iterate(self.eval(c.named_children[0], env, module)))
            elif c.type != "comment":
                args.append(self.eval(c, env, module))
        return args

    def eval_call(self, n, env, module):
        fn_node = n.child_by_field_name("function")
        args_node = n.child_by_field_name("arguments")
        if args_node.type == "template_string":
            raise Unresolved("tagged template")
        arg_nodes = [c for c in args_node.named_children if c.type != "comment"]
        # A feature flag read: getter("tengu_name", default). Flags are fetched
        # from a server at run time, so the value compiled in as the default is
        # used and the read is recorded.
        if len(arg_nodes) == 2 and arg_nodes[0].type == "string":
            flag = self.eval(arg_nodes[0], env, module)
            if flag.startswith("tengu_"):
                default = self.eval(arg_nodes[1], env, module)
                try:
                    self.flags[flag] = to_python(default)
                except Unresolved:
                    self.flags[flag] = "<not JSON>"
                return default
        optional_call = any(c.type == "optional_chain" for c in n.children)
        if fn_node.type == "member_expression":
            obj = self.chain_object(fn_node.child_by_field_name("object"), env, module)
            if obj is SHORT:
                return SHORT
            if (obj is UNDEF or obj is None) and any(c.type == "optional_chain" for c in fn_node.children):
                return SHORT
            key = text(fn_node.child_by_field_name("property"))
            callee = self.get_member(obj, key)
            this = obj
        elif fn_node.type == "import":
            return self.namespace(self.eval(arg_nodes[0], env, module)) if len(arg_nodes) == 1 else Unknown("a dynamic import")
        else:
            callee = self.chain_object(fn_node, env, module)
            if callee is SHORT:
                return SHORT
            this = UNDEF
        if (callee is UNDEF or callee is None) and optional_call:
            return SHORT
        args = self.eval_args(args_node, env, module)
        # The same flag read through a wrapper that passes the name along.
        if len(args) == 2 and isinstance(args[0], str) and args[0].startswith("tengu_") and isinstance(callee, Function):
            try:
                self.flags[args[0]] = to_python(args[1])
            except Unresolved:
                self.flags[args[0]] = "<not JSON>"
            return args[1]
        return self.call(callee, this, args, text(fn_node)[:60])

    def eval_new(self, n, env, module):
        ctor_node = n.child_by_field_name("constructor")
        args_node = n.child_by_field_name("arguments")
        args = self.eval_args(args_node, env, module) if args_node is not None else []
        name = text(ctor_node)
        if name == "Set":
            return JSSet(self.iterate(args[0]) if args else [])
        if name == "Map":
            return JSMap([(p[0], p[1]) for p in self.iterate(args[0])] if args else [])
        ctor = self.eval(ctor_node, env, module)
        if isinstance(ctor, Unknown):
            return Unknown(f"an instance of {name}", origin=("new", ctor.origin))
        if isinstance(ctor, (Function, Builtin)):
            # A constructor function or runtime class: its instances are not modelled.
            return Unknown(f"an instance of {name}", origin=("new", id(ctor)))
        if not isinstance(ctor, JSClass):
            raise Unresolved(f"new on {type(ctor).__name__}")
        body = ctor.node.child_by_field_name("body")
        if any(c.type == "class_heritage" for c in ctor.node.children):
            raise Unresolved("class with a superclass")
        instance = JSObject()
        class_env = Env(ctor.env)
        class_env.vars["this"] = instance
        constructor = None
        for member in body.named_children:
            if member.type == "field_definition":
                if any(c.type == "static" for c in member.children):
                    continue
                value = member.child_by_field_name("value")
                key = text(member.child_by_field_name("property"))
                instance.props[key] = self.eval(value, class_env, ctor.module) if value is not None else UNDEF
            elif member.type == "method_definition":
                key = text(member.child_by_field_name("name"))
                fn = Function(self, member, ctor.env, ctor.module, name=key)
                if key == "constructor":
                    constructor = fn
                elif any(c.type == "get" for c in member.children):
                    instance.props[key] = Getter(lambda this, f=fn: f.call(this, []))
                else:
                    instance.props[key] = fn
        if constructor is not None:
            constructor.call(instance, args)
        return instance

    def eval_binary(self, n, env, module):
        op = text(n.child_by_field_name("operator"))
        left = self.eval(n.child_by_field_name("left"), env, module)
        right_node = n.child_by_field_name("right")
        left_node = n.child_by_field_name("left")
        if op == "&&":
            return self.eval(right_node, env, module) if self.truth(left, left_node, module) else left
        if op == "||":
            return left if self.truth(left, left_node, module) else self.eval(right_node, env, module)
        if op == "??":
            if isinstance(left, Unknown):
                if not self.decide(left_node, module, left, "is null or undefined"):
                    return left
                left = UNDEF
            return self.eval(right_node, env, module) if left is UNDEF or left is None else left
        right = self.eval(right_node, env, module)
        if op in ("===", "!==", "==", "!="):
            unknown = left if isinstance(left, Unknown) else right if isinstance(right, Unknown) else None
            if unknown is not None:
                return self.decide(n, module, unknown, "is true")
        if op == "===":
            return strict_equal(left, right)
        if op == "!==":
            return not strict_equal(left, right)
        for side in (left, right):
            if isinstance(side, Unknown):
                if op in ("<", ">", "<=", ">="):
                    return self.decide(n, module, side, "is true")
                raise Unresolved(f"operator {op} on {side.why}")
        if op in ("==", "!="):
            nullish = (left is UNDEF or left is None) and (right is UNDEF or right is None)
            equal = nullish or (type(left) is type(right) and strict_equal(left, right))
            return equal if op == "==" else not equal
        if op == "+":
            if isinstance(left, str) or isinstance(right, str):
                return to_string(left) + to_string(right)
        numeric = all(isinstance(x, (int, float)) and not isinstance(x, bool) for x in (left, right))
        if numeric:
            if op == "+":
                return left + right
            if op == "-":
                return left - right
            if op == "*":
                return left * right
            if op == "/":
                if right == 0:
                    return math.inf if left > 0 else -math.inf if left < 0 else math.nan
                result = left / right
                return int(result) if result == int(result) else result
            if op == "%":
                return math.fmod(left, right)
            if op == "**":
                return left**right
            if op == "<":
                return left < right
            if op == ">":
                return left > right
            if op == "<=":
                return left <= right
            if op == ">=":
                return left >= right
        if op == "in" and isinstance(right, JSObject):
            return to_string(left) in right.props
        raise Unresolved(f"unsupported operator {op} on {type(left).__name__} and {type(right).__name__}")


def pattern_names(pattern):
    """Every identifier a destructuring pattern binds."""
    t = pattern.type
    if t in ("identifier", "shorthand_property_identifier_pattern"):
        return [text(pattern)]
    if t in ("assignment_pattern", "object_assignment_pattern"):
        return pattern_names(pattern.child_by_field_name("left"))
    if t == "pair_pattern":
        return pattern_names(pattern.child_by_field_name("value"))
    if t in ("object_pattern", "array_pattern", "rest_pattern"):
        return [name for part in pattern.named_children for name in pattern_names(part)]
    raise Unresolved(f"unsupported pattern {t}")


def typeof(v):
    if isinstance(v, Unknown):
        raise Unresolved(f"typeof {v.why}")
    if v is UNDEF:
        return "undefined"
    if isinstance(v, bool):
        return "boolean"
    if isinstance(v, str):
        return "string"
    if isinstance(v, (int, float)):
        return "number"
    if isinstance(v, (Function, Builtin, JSClass)):
        return "function"
    return "object"


def string_member(interp, s, key):
    def replace(pattern, replacement, every=False):
        if isinstance(pattern, JSRegex):
            flags = regex.MULTILINE if "m" in pattern.flags else 0
            if "i" in pattern.flags:
                flags |= regex.IGNORECASE
            if not isinstance(replacement, str) or "$" in replacement:
                raise Unresolved("regex replace with a callback or a group reference")
            try:
                # The regex module, for \\p{...} classes that re lacks.
                compiled = regex.compile(pattern.source, flags)
            except regex.error as e:
                raise Unresolved(f"regex {pattern.source!r} is not portable: {e}") from e
            return compiled.sub(lambda _: replacement, s, count=0 if "g" in pattern.flags or every else 1)
        if not isinstance(pattern, str) or not isinstance(replacement, str):
            raise Unresolved("string replace with a runtime value")
        return s.replace(pattern, replacement) if every else s.replace(pattern, replacement, 1)

    table = {
        "length": len(s.encode("utf-16-le")) // 2,
        "trim": Builtin(lambda: s.strip(" \t\n\r\v\f\u00a0\ufeff\u2028\u2029")),
        "trimEnd": Builtin(lambda: s.rstrip(" \t\n\r\v\f\u00a0\ufeff\u2028\u2029")),
        "trimStart": Builtin(lambda: s.lstrip(" \t\n\r\v\f\u00a0\ufeff\u2028\u2029")),
        "toLowerCase": Builtin(lambda: s.lower()),
        "toUpperCase": Builtin(lambda: s.upper()),
        "startsWith": Builtin(lambda p: s.startswith(to_string(p))),
        "endsWith": Builtin(lambda p: s.endswith(to_string(p))),
        "includes": Builtin(lambda p: to_string(p) in s),
        "indexOf": Builtin(lambda p: s.find(to_string(p))),
        "split": Builtin(lambda sep: s.split(to_string(sep)) if to_string(sep) != "" else list(s)),
        "slice": Builtin(lambda a=0, b=None: s[int(a) : (None if b is None or b is UNDEF else int(b))]),
        "replace": Builtin(replace),
        "replaceAll": Builtin(lambda p, r: replace(p, r, every=True)),
        "repeat": Builtin(lambda count: s * int(count)),
        "toString": Builtin(lambda: s),
        "normalize": Builtin(lambda form="NFC": unicodedata.normalize(to_string(form), s)),
    }
    if key not in table:
        raise Unresolved(f"unsupported string member {key}")
    return table[key]


def array_member(interp, a, key):
    def fn(f):
        return lambda *args: f.call(UNDEF, list(args))

    def flat_map(f):
        out = []
        for i, x in enumerate(a):
            r = f.call(UNDEF, [x, i])
            if isinstance(r, list):
                out.extend(r)
            else:
                out.append(r)
        return out

    def find(f):
        for i, x in enumerate(a):
            if truthy(f.call(UNDEF, [x, i])):
                return x
        return UNDEF

    def flat():
        out = []
        for x in a:
            if isinstance(x, list):
                out.extend(x)
            else:
                out.append(x)
        return out

    def push(*items):
        a.extend(items)
        return len(a)

    table = {
        "length": len(a),
        "map": Builtin(lambda f: [f.call(UNDEF, [x, i]) for i, x in enumerate(a)]),
        "filter": Builtin(lambda f: [x for i, x in enumerate(a) if truthy(f.call(UNDEF, [x, i]))]),
        "flatMap": Builtin(flat_map),
        "flat": Builtin(flat),
        "some": Builtin(lambda f: any(truthy(f.call(UNDEF, [x, i])) for i, x in enumerate(a))),
        "every": Builtin(lambda f: all(truthy(f.call(UNDEF, [x, i])) for i, x in enumerate(a))),
        "find": Builtin(find),
        "includes": Builtin(lambda v: any(strict_equal(v, x) for x in a)),
        "join": Builtin(
            lambda sep=",": to_string(sep).join("" if x is None or x is UNDEF else to_string(x) for x in a)
        ),
        "concat": Builtin(lambda *others: a + [y for o in others for y in (o if isinstance(o, list) else [o])]),
        "slice": Builtin(lambda s=0, e=None: a[int(s) : (None if e is None or e is UNDEF else int(e))]),
        "push": Builtin(push),
        "entries": Builtin(lambda: [[i, x] for i, x in enumerate(a)]),
    }
    if key not in table:
        raise Unresolved(f"unsupported array member {key}")
    return table[key]


def _object_namespace():
    obj = JSObject()

    def entries(o):
        if not isinstance(o, JSObject):
            raise Unresolved("Object.entries of a runtime value")
        return [[k, o.get(k)] for k in o.keys()]

    def from_entries(pairs):
        out = JSObject()
        for p in pairs:
            out.props[to_string(p[0])] = p[1]
        return out

    def keys(o):
        if not isinstance(o, JSObject):
            raise Unresolved("Object.keys of a runtime value")
        return o.keys()

    def freeze(o):
        return o

    obj.props.update(
        entries=Builtin(entries),
        fromEntries=Builtin(from_entries),
        keys=Builtin(keys),
        values=Builtin(lambda o: [o.get(k) for k in keys(o)]),
        freeze=Builtin(freeze),
    )
    return obj


def _array_namespace():
    obj = JSObject()

    def is_array(v):
        if isinstance(v, Unknown):
            raise Unresolved(f"Array.isArray of {v.why}")
        return isinstance(v, list)

    obj.props.update(isArray=Builtin(is_array), **{"from": Builtin(lambda v: list(v.items) if isinstance(v, JSSet) else list(v))})
    return obj


def _number_namespace():
    obj = JSObject()
    obj.props.update(
        MAX_SAFE_INTEGER=2**53 - 1,
        MIN_SAFE_INTEGER=-(2**53 - 1),
        POSITIVE_INFINITY=math.inf,
        NEGATIVE_INFINITY=-math.inf,
    )
    return obj


def _math_namespace():
    obj = JSObject()
    obj.props.update(
        max=Builtin(lambda *a: max(a)),
        min=Builtin(lambda *a: min(a)),
        floor=Builtin(lambda v: math.floor(v)),
        ceil=Builtin(lambda v: math.ceil(v)),
        round=Builtin(lambda v: math.floor(v + 0.5)),
    )
    return obj


def _boolean(v=UNDEF):
    return truthy(v)


def _string(v=""):
    return to_string(v)


def _promise_namespace():
    obj = JSObject()
    # await is evaluated as its operand, so a promise is its value.
    obj.props.update(all=Builtin(lambda values: list(values)), resolve=Builtin(lambda v=UNDEF: v))
    return obj


GLOBALS = {
    "Promise": _promise_namespace(),
    "Object": _object_namespace(),
    "Array": _array_namespace(),
    "Number": _number_namespace(),
    "Math": _math_namespace(),
    "Boolean": Builtin(_boolean),
    "String": Builtin(_string),
    "Infinity": math.inf,
    "NaN": math.nan,
}
