"""Zod 4 schema construction and its JSON Schema conversion, re-implemented.

Claude Code declares tool inputs as zod schemas and converts them when it
builds a request. The conversion is library code in the bundle (`toJSONSchema`
plus Claude Code's wrapper around it), not text that can be copied out, so it
is ported here. Key order follows the library, because the order is part of
what the model receives.

Anything the port does not cover raises Unresolved, and the tool is then left
out of the capture rather than emitted with a guessed schema.
"""

import re

DRAFT = "https://json-schema.org/draft/2020-12/schema"
MAX_SAFE = 2**53 - 1

# Number formats and their bounds, from zod's NUMBER_FORMAT_RANGES.
NUMBER_RANGES = {
    "safeint": (-MAX_SAFE, MAX_SAFE),
    "int32": (-2147483648, 2147483647),
    "uint32": (0, 4294967295),
}

WRAPPERS = ("optional", "nullable", "default", "prefault", "nonoptional", "catch", "readonly")


def _jseval():
    import jseval

    return jseval


def unresolved(msg):
    return _jseval().Unresolved(f"zod: {msg}")


class Schema:
    def __init__(self, type_, parent=None, checks=(), **fields):
        self.type = type_
        self.fields = fields
        self.checks = list(checks)
        self.parent = parent
        self.meta = None

    def clone(self, parent=True, **changes):
        fields = dict(self.fields)
        checks = changes.pop("checks", self.checks)
        fields.update(changes)
        return Schema(self.type, parent=self if parent else None, checks=checks, **fields)


def function_body(source, start):
    depth = 0
    for i in range(start, len(source)):
        ch = source[i]
        if ch == "{":
            depth += 1
        elif ch == "}":
            depth -= 1
            if depth == 0:
                return source[start + 1 : i]
    return ""


def classify_module(source):
    """Map each schema factory function of a zod module to what it builds.

    Returns {} for a module that is not this zod build."""
    factory = re.search(r'([\w$]+)\("ZodString",', source)
    if factory is None or '"ZodObject"' not in source:
        return {}
    classes = dict(re.findall(r"([\w$]+)=" + re.escape(factory.group(1)) + r'\("(Zod\w+)",', source))
    if not classes:
        return {}
    bodies = {}
    for m in re.finditer(r"function ([\w$]+)\(([^)]*)\)\{", source):
        bodies[m.group(1)] = function_body(source, m.end() - 1)
    token = re.compile(r"[\w$]+")

    def first_class(body):
        for word in token.findall(body):
            if word in classes:
                return classes[word]
        return None

    out = {}
    for name, body in bodies.items():
        cls = first_class(body)
        if cls is None or "return" not in body:
            continue
        kind = {"class": cls}
        helper = re.match(r"return ([\w$]+)\(", body)
        text = body + (bodies.get(helper.group(1), "") if helper else "")
        fmt = re.search(r'format:"([\w-]+)"', text)
        if fmt:
            kind["format"] = fmt.group(1)
        if "coerce:!0" in text:
            kind["coerce"] = True
        if cls == "ZodObject":
            catchall = re.search(r"catchall:([\w$]+)\(\)", body)
            if catchall:
                inner = first_class(bodies.get(catchall.group(1), ""))
                kind["catchall"] = {"ZodNever": "never", "ZodUnknown": "unknown"}.get(inner, "other")
        if cls == "ZodRecord" and "values=void 0" in body:
            kind["partial"] = True
        out[name] = kind
    return out


class Zod:
    # -- construction ----------------------------------------------------

    def build(self, kind, args, interp):
        js = _jseval()
        cls = kind["class"]
        if kind.get("coerce"):
            raise unresolved(f"coercing {cls}")

        def arg(i):
            return args[i] if i < len(args) else js.UNDEF

        def schema_arg(i):
            v = arg(i)
            if not isinstance(v, Schema):
                raise unresolved(f"{cls} argument {i} is not a schema")
            return v

        simple = {
            "ZodString": "string",
            "ZodNumber": "number",
            "ZodBoolean": "boolean",
            "ZodNull": "null",
            "ZodUndefined": "undefined",
            "ZodAny": "any",
            "ZodUnknown": "unknown",
            "ZodNever": "never",
            "ZodCustom": "custom",
            "ZodTransform": "transform",
        }
        if cls in simple:
            return Schema(simple[cls])
        if cls == "ZodNumberFormat":
            return Schema("number", checks=[("number_format", kind.get("format"))])
        if cls == "ZodObject":
            shape = arg(0)
            if shape is js.UNDEF or shape is None:
                shape = js.JSObject()
            if not isinstance(shape, js.JSObject):
                raise unresolved("object shape is not a literal object")
            return Schema("object", shape=self.shape_of(shape), catchall=kind.get("catchall"))
        if cls == "ZodUnion":
            return Schema("union", options=self.schema_list(arg(0)))
        if cls == "ZodDiscriminatedUnion":
            return Schema("union", options=self.schema_list(arg(1)))
        if cls == "ZodIntersection":
            return Schema("intersection", left=schema_arg(0), right=schema_arg(1))
        if cls == "ZodArray":
            return Schema("array", element=schema_arg(0))
        if cls == "ZodTuple":
            rest = arg(1) if isinstance(arg(1), Schema) else None
            return Schema("tuple", items=self.schema_list(arg(0)), rest=rest)
        if cls == "ZodRecord":
            if kind.get("partial"):
                raise unresolved("partial record")
            if isinstance(arg(1), Schema):
                return Schema("record", key=schema_arg(0), value=schema_arg(1))
            return Schema("record", key=Schema("string"), value=schema_arg(0))
        if cls == "ZodEnum":
            entries = arg(0)
            if isinstance(entries, list):
                values = list(entries)
            elif isinstance(entries, js.JSObject):
                all_values = [entries.get(k) for k in entries.keys()]
                numeric = [v for v in all_values if isinstance(v, (int, float)) and not isinstance(v, bool)]
                values = [entries.get(k) for k in entries.keys() if not _is_numeric_key(k, numeric)]
            else:
                raise unresolved("enum entries are not literal")
            return Schema("enum", values=[js.to_python(v) for v in values])
        if cls == "ZodLiteral":
            values = arg(0) if isinstance(arg(0), list) else [arg(0)]
            if any(v is js.UNDEF for v in values):
                raise unresolved("literal undefined")
            return Schema("literal", values=[js.to_python(v) for v in values])
        if cls in ("ZodOptional", "ZodExactOptional"):
            return Schema("optional", inner=schema_arg(0))
        if cls == "ZodNullable":
            return Schema("nullable", inner=schema_arg(0))
        if cls == "ZodNonOptional":
            return Schema("nonoptional", inner=schema_arg(0))
        if cls == "ZodReadonly":
            return Schema("readonly", inner=schema_arg(0))
        if cls in ("ZodDefault", "ZodPrefault"):
            value = arg(1)
            if isinstance(value, (js.Function, js.Builtin)):
                value = value.call(js.UNDEF, [])
            return Schema("default" if cls == "ZodDefault" else "prefault", inner=schema_arg(0), value=js.to_python(value))
        if cls == "ZodCatch":
            raise unresolved("catch")
        if cls == "ZodLazy":
            return Schema("lazy", getter=arg(0))
        if cls == "ZodPreprocess":
            # preprocess(fn, schema): a pipe from a transform into the schema.
            return Schema("pipe", input=Schema("transform"), output=schema_arg(1))
        if cls == "ZodPipe":
            return Schema("pipe", input=schema_arg(0), output=schema_arg(1))
        if cls == "ZodURL":
            return Schema("string", checks=[("string_format", "url")])
        raise unresolved(f"unsupported schema class {cls}")

    def shape_of(self, obj):
        js = _jseval()
        shape = {}
        for key in obj.keys():
            value = obj.get(key)
            if not isinstance(value, Schema):
                if isinstance(value, js.Unknown):
                    raise unresolved(f"property {key} depends on {value.why}")
                raise unresolved(f"property {key} is not a schema")
            shape[key] = value
        return shape

    def schema_list(self, value):
        if not isinstance(value, list) or not all(isinstance(v, Schema) for v in value):
            raise unresolved("expected a literal list of schemas")
        return list(value)

    # -- methods ---------------------------------------------------------

    def member(self, s, key, interp):
        js = _jseval()
        B = js.Builtin

        def check(*c):
            return s.clone(checks=s.checks + list(c))

        def with_meta(meta):
            out = s.clone()
            out.meta = meta
            return out

        def describe(text):
            if not isinstance(text, str):
                raise unresolved("description is not a constant string")
            return with_meta({"description": text})

        def meta(obj=js.UNDEF):
            if obj is js.UNDEF:
                raise unresolved("meta() read")
            return with_meta(js.to_python(obj))

        def default(value, type_="default"):
            if isinstance(value, (js.Function, js.Builtin)):
                value = value.call(js.UNDEF, [])
            return Schema(type_, inner=s, value=js.to_python(value))

        def num(v):
            if isinstance(v, bool) or not isinstance(v, (int, float)):
                raise unresolved("bound is not a constant number")
            return v

        common = {
            "describe": B(describe),
            "meta": B(meta),
            "optional": B(lambda: Schema("optional", inner=s)),
            "exactOptional": B(lambda: Schema("optional", inner=s)),
            "nullable": B(lambda: Schema("nullable", inner=s)),
            "nullish": B(lambda: Schema("optional", inner=Schema("nullable", inner=s))),
            "nonoptional": B(lambda *_: Schema("nonoptional", inner=s)),
            "readonly": B(lambda: Schema("readonly", inner=s)),
            "default": B(default),
            "prefault": B(lambda v: default(v, "prefault")),
            "array": B(lambda: Schema("array", element=s)),
            "or": B(lambda other: Schema("union", options=[s, other])),
            "and": B(lambda other: Schema("intersection", left=s, right=other)),
            "pipe": B(lambda other: Schema("pipe", input=s, output=other)),
            "transform": B(lambda *_: Schema("pipe", input=s, output=Schema("transform"))),
            # Refinements validate values and add nothing to the JSON Schema.
            "refine": B(lambda *_: check()),
            "superRefine": B(lambda *_: check()),
            "brand": B(lambda *_: s),
        }
        if key in common:
            return common[key]
        t = s.type
        if t == "string":
            table = {
                "min": lambda v, *_: check(("min_length", num(v))),
                "max": lambda v, *_: check(("max_length", num(v))),
                "length": lambda v, *_: check(("length_equals", num(v))),
                "nonempty": lambda *_: check(("min_length", 1)),
                "regex": lambda r, *_: check(("regex", _regex_source(r))),
                "url": lambda *_: check(("string_format", "url")),
                "trim": lambda: check(),
                "toLowerCase": lambda: check(),
                "toUpperCase": lambda: check(),
                "normalize": lambda *_: check(),
            }
            if key in table:
                return B(table[key])
        if t == "number":
            table = {
                "gt": lambda v, *_: check(("greater_than", num(v), False)),
                "gte": lambda v, *_: check(("greater_than", num(v), True)),
                "min": lambda v, *_: check(("greater_than", num(v), True)),
                "lt": lambda v, *_: check(("less_than", num(v), False)),
                "lte": lambda v, *_: check(("less_than", num(v), True)),
                "max": lambda v, *_: check(("less_than", num(v), True)),
                "int": lambda *_: check(("number_format", "safeint")),
                "safe": lambda *_: check(("number_format", "safeint")),
                "positive": lambda *_: check(("greater_than", 0, False)),
                "nonnegative": lambda *_: check(("greater_than", 0, True)),
                "negative": lambda *_: check(("less_than", 0, False)),
                "nonpositive": lambda *_: check(("less_than", 0, True)),
                "multipleOf": lambda v, *_: check(("multiple_of", num(v))),
                "step": lambda v, *_: check(("multiple_of", num(v))),
                "finite": lambda *_: s,
            }
            if key in table:
                return B(table[key])
        if t == "array":
            table = {
                "min": lambda v, *_: check(("min_length", num(v))),
                "max": lambda v, *_: check(("max_length", num(v))),
                "length": lambda v, *_: check(("length_equals", num(v))),
                "nonempty": lambda *_: check(("min_length", 1)),
                "unwrap": lambda: s.fields["element"],
            }
            if key in table:
                return B(table[key])
            if key == "element":
                return s.fields["element"]
        if t == "object":
            shape = s.fields["shape"]

            def reshape(new_shape, **changes):
                return s.clone(parent=False, checks=[], shape=new_shape, **changes)

            def mask_keys(mask):
                if not isinstance(mask, js.JSObject):
                    raise unresolved("mask is not a literal object")
                for k in mask.keys():
                    if k not in shape:
                        raise unresolved(f"unrecognised key {k} in mask")
                return [k for k in mask.keys() if js.truthy(mask.get(k))]

            def extend(extra):
                if isinstance(extra, Schema):
                    extra_shape = extra.fields["shape"]
                elif isinstance(extra, js.JSObject):
                    extra_shape = self.shape_of(extra)
                else:
                    raise unresolved("extend argument is not a literal object")
                return reshape({**shape, **extra_shape})

            def partial(mask=js.UNDEF):
                keys = list(shape) if mask is js.UNDEF else mask_keys(mask)
                return reshape({k: (Schema("optional", inner=v) if k in keys else v) for k, v in shape.items()})

            def as_object():
                obj = js.JSObject()
                obj.props.update(shape)
                return obj

            table = {
                "extend": extend,
                "safeExtend": extend,
                "merge": extend,
                "pick": lambda mask: reshape({k: shape[k] for k in mask_keys(mask)}),
                "omit": lambda mask: (lambda drop: reshape({k: v for k, v in shape.items() if k not in drop}))(mask_keys(mask)),
                "partial": partial,
                "strict": lambda: s.clone(parent=False, catchall="never"),
                "strip": lambda: s.clone(parent=False, catchall=None),
                "passthrough": lambda: s.clone(parent=False, catchall="unknown"),
                "loose": lambda: s.clone(parent=False, catchall="unknown"),
                "keyof": lambda: Schema("enum", values=list(shape)),
            }
            if key in table:
                return B(table[key])
            if key == "shape":
                return as_object()
        if t in WRAPPERS and key == "unwrap":
            return B(lambda: s.fields["inner"])
        if t == "union" and key == "options":
            return list(s.fields["options"])
        if t == "enum" and key == "options":
            return list(s.fields["values"])
        raise unresolved(f"unsupported member .{key} on a {t} schema")

    # -- metadata and optionality ---------------------------------------

    def metadata(self, s):
        if s.parent is not None:
            inherited = dict(self.metadata(s.parent) or {})
            inherited.pop("id", None)
            merged = {**inherited, **(s.meta or {})}
            return merged or None
        return s.meta

    def lazy_inner(self, s):
        js = _jseval()
        if "resolved" not in s.fields:
            getter = s.fields["getter"]
            if not isinstance(getter, (js.Function, js.Builtin)):
                raise unresolved("lazy getter is not a function")
            inner = getter.call(js.UNDEF, [])
            if not isinstance(inner, Schema):
                raise unresolved("lazy getter did not return a schema")
            s.fields["resolved"] = inner
        return s.fields["resolved"]

    def optout(self, s):
        """zod's `_zod.optout === "optional"`."""
        t = s.type
        if t == "optional":
            return True
        if t in ("nullable", "readonly", "prefault", "catch"):
            return self.optout(s.fields["inner"])
        if t == "lazy":
            return self.optout(self.lazy_inner(s))
        if t == "pipe":
            return self.optout(s.fields["output"])
        if t == "union":
            return any(self.optout(o) for o in s.fields["options"])
        return False

    def is_optional(self, s):
        """Claude Code's own test for leaving a property out of `required`."""
        t = s.type
        if t == "undefined":
            return True
        if t == "transform":
            return False
        if t == "pipe":
            return self.is_optional(s.fields["output"])
        if t == "union":
            return any(self.is_optional(o) for o in s.fields["options"])
        if t in ("nullable", "readonly"):
            return self.is_optional(s.fields["inner"])
        if t == "catch":
            return self.is_optional(s.fields["inner"])
        if t == "lazy":
            return self.is_optional(self.lazy_inner(s))
        return self.optout(s)

    # -- JSON Schema -----------------------------------------------------

    def to_json_schema(self, root):
        """`toJSONSchema(schema, {unrepresentable: "throw"})` as Claude Code
        wraps it: draft 2020-12, output mode."""
        seen = {}
        order = []

        def process(s, stack):
            entry = seen.get(id(s))
            if entry is not None:
                if any(x is s for x in stack):
                    raise unresolved("recursive schema, which needs $defs")
                return entry["schema"]
            entry = {"schema": {}, "ref": None, "zod": s}
            seen[id(s)] = entry
            order.append(entry)
            self.emit(s, entry, lambda child: process(child, stack + [s]))
            meta = self.metadata(s)
            if meta:
                if "id" in meta:
                    raise unresolved("schema with a registered id, which needs $defs")
                entry["schema"].update(meta)
            return entry["schema"]

        process(root, [])

        done = set()

        def finish(entry):
            if id(entry) in done:
                return
            done.add(id(entry))
            schema = entry["schema"]
            own = dict(schema)
            ref = entry["ref"]
            if ref is not None:
                target = seen[id(ref)]
                finish(target)
                schema.update(target["schema"])
                schema.update(own)
            self.override(entry["zod"], schema)

        for entry in reversed(order):
            finish(entry)
        return {"$schema": DRAFT, **seen[id(root)]["schema"]}

    def override(self, s, schema):
        t = s.type
        if t in ("custom", "transform"):
            raise unresolved(f"{t} cannot be represented in JSON Schema")
        if t == "undefined":
            schema["not"] = {}
        elif t == "object" and "properties" in schema and "$ref" not in schema:
            shape = s.fields["shape"]
            required = [k for k in shape if not self.is_optional(shape[k])]
            had_additional = "additionalProperties" in schema
            additional = schema.pop("additionalProperties", None)
            schema.pop("required", None)
            if required:
                schema["required"] = required
            if had_additional:
                schema["additionalProperties"] = additional
        meta = self.metadata(s)
        if meta:
            # Registered metadata comes first, as upstream reorders it.
            for key in [k for k in schema if k not in meta]:
                schema[key] = schema.pop(key)

    def emit(self, s, entry, process):
        t = s.type
        out = entry["schema"]
        f = s.fields
        if t == "string":
            bag = self.string_bag(s)
            out["type"] = "string"
            if "minimum" in bag:
                out["minLength"] = bag["minimum"]
            if "maximum" in bag:
                out["maxLength"] = bag["maximum"]
            if "format" in bag:
                out["format"] = {"url": "uri"}[bag["format"]]
            patterns = bag.get("patterns", [])
            if len(patterns) == 1:
                out["pattern"] = patterns[0]
            elif len(patterns) > 1:
                out["allOf"] = [{"pattern": p} for p in patterns]
        elif t == "number":
            bag = self.number_bag(s)
            out["type"] = "integer" if "int" in bag.get("format", "") else "number"
            minimum, maximum = bag.get("minimum"), bag.get("maximum")
            excl_min, excl_max = bag.get("exclusiveMinimum"), bag.get("exclusiveMaximum")
            if excl_min is not None and excl_min >= (minimum if minimum is not None else float("-inf")):
                out["exclusiveMinimum"] = excl_min
            elif minimum is not None:
                out["minimum"] = minimum
            if excl_max is not None and excl_max <= (maximum if maximum is not None else float("inf")):
                out["exclusiveMaximum"] = excl_max
            elif maximum is not None:
                out["maximum"] = maximum
            if "multipleOf" in bag:
                out["multipleOf"] = bag["multipleOf"]
        elif t == "boolean":
            out["type"] = "boolean"
        elif t == "null":
            out["type"] = "null"
        elif t == "never":
            out["not"] = {}
        elif t in ("any", "unknown", "undefined", "custom", "transform"):
            pass
        elif t == "enum":
            values = f["values"]
            if all(isinstance(v, (int, float)) and not isinstance(v, bool) for v in values):
                out["type"] = "number"
            if all(isinstance(v, str) for v in values):
                out["type"] = "string"
            out["enum"] = list(values)
        elif t == "literal":
            values = f["values"]
            if len(values) == 1:
                out["type"] = "null" if values[0] is None else _typeof(values[0])
                out["const"] = values[0]
            elif values:
                for name, test in (("number", _is_number), ("string", lambda v: isinstance(v, str)), ("boolean", lambda v: isinstance(v, bool))):
                    if all(test(v) for v in values):
                        out["type"] = name
                if all(v is None for v in values):
                    out["type"] = "null"
                out["enum"] = list(values)
        elif t == "array":
            bag = self.length_bag(s)
            if "minimum" in bag:
                out["minItems"] = bag["minimum"]
            if "maximum" in bag:
                out["maxItems"] = bag["maximum"]
            out["type"] = "array"
            out["items"] = process(f["element"])
        elif t == "object":
            out["type"] = "object"
            out["properties"] = {}
            for key, value in f["shape"].items():
                out["properties"][key] = process(value)
            required = [k for k, v in f["shape"].items() if not self.optout(v)]
            if required:
                out["required"] = required
            catchall = f.get("catchall")
            if catchall in ("never", None):
                out["additionalProperties"] = False
            elif catchall == "unknown":
                out["additionalProperties"] = {}
            else:
                raise unresolved("object catchall that is neither never nor unknown")
        elif t == "union":
            out["anyOf"] = [process(o) for o in f["options"]]
        elif t == "intersection":
            parts = []
            for side in (process(f["left"]), process(f["right"])):
                parts.extend(side["allOf"] if list(side) == ["allOf"] else [side])
            out["allOf"] = parts
        elif t == "tuple":
            out["type"] = "array"
            out["prefixItems"] = [process(i) for i in f["items"]]
            if f["rest"] is not None:
                out["items"] = process(f["rest"])
        elif t == "record":
            out["type"] = "object"
            key = f["key"]
            if key.type not in ("string", "enum"):
                raise unresolved(f"record keyed by {key.type}")
            out["propertyNames"] = process(key)
            out["additionalProperties"] = process(f["value"])
            if key.type == "enum":
                out["required"] = list(key.fields["values"])
        elif t == "nullable":
            out["anyOf"] = [process(f["inner"]), {"type": "null"}]
        elif t in ("optional", "nonoptional", "prefault"):
            process(f["inner"])
            entry["ref"] = f["inner"]
        elif t == "default":
            process(f["inner"])
            entry["ref"] = f["inner"]
            out["default"] = f["value"]
        elif t == "readonly":
            process(f["inner"])
            entry["ref"] = f["inner"]
            out["readOnly"] = True
        elif t == "lazy":
            inner = self.lazy_inner(s)
            process(inner)
            entry["ref"] = inner
        elif t == "pipe":
            process(f["output"])
            entry["ref"] = f["output"]
        else:
            raise unresolved(f"no JSON Schema rule for {t}")

    def number_bag(self, s):
        bag = {}
        for c in s.checks:
            kind = c[0]
            if kind == "number_format":
                if c[1] not in NUMBER_RANGES:
                    raise unresolved(f"number format {c[1]}")
                bag["format"] = c[1]
                bag["minimum"], bag["maximum"] = NUMBER_RANGES[c[1]]
            elif kind == "greater_than":
                field = "minimum" if c[2] else "exclusiveMinimum"
                if c[1] > bag.get(field, float("-inf")):
                    bag[field] = c[1]
            elif kind == "less_than":
                field = "maximum" if c[2] else "exclusiveMaximum"
                if c[1] < bag.get(field, float("inf")):
                    bag[field] = c[1]
            elif kind == "multiple_of":
                bag.setdefault("multipleOf", c[1])
            else:
                raise unresolved(f"number check {kind}")
        return bag

    def length_bag(self, s):
        bag = {}
        for c in s.checks:
            kind = c[0]
            if kind == "min_length":
                if c[1] > bag.get("minimum", float("-inf")):
                    bag["minimum"] = c[1]
            elif kind == "max_length":
                if c[1] < bag.get("maximum", float("inf")):
                    bag["maximum"] = c[1]
            elif kind == "length_equals":
                bag["minimum"] = bag["maximum"] = c[1]
            elif kind not in ("regex", "string_format"):
                raise unresolved(f"length check {kind}")
        return bag

    def string_bag(self, s):
        bag = self.length_bag(s)
        for c in s.checks:
            if c[0] == "regex":
                patterns = bag.setdefault("patterns", [])
                if c[1] not in patterns:
                    patterns.append(c[1])
            elif c[0] == "string_format":
                if c[1] != "url":
                    raise unresolved(f"string format {c[1]}")
                bag["format"] = c[1]
        return bag


def _regex_source(r):
    js = _jseval()
    if not isinstance(r, js.JSRegex):
        raise unresolved("regex() argument is not a regex literal")
    return r.source


def _is_number(v):
    return isinstance(v, (int, float)) and not isinstance(v, bool)


def _typeof(v):
    if isinstance(v, bool):
        return "boolean"
    if isinstance(v, str):
        return "string"
    if _is_number(v):
        return "number"
    raise unresolved("literal of an unsupported type")


def _is_numeric_key(key, numeric_values):
    try:
        return float(key) in numeric_values
    except ValueError:
        return False
