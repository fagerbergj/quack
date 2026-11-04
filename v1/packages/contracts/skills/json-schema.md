# JSON Schema contract guide

Sources: JSON Schema 2020-12, Google AIP-126, 140, 142, 180, 203. A rule marked check is enforced mechanically; a rule marked ask is a choice for the person, with a suggested default and its trade-off. Use this kind for config files, artifact schemas and durable document shapes.

## Intent first

Settle these before drafting: who writes the document and who reads it (they may be different programs), where it lives (a file, a stored document, an artifact), how long old copies stay readable, and what happens when a reader meets a field it does not know. Read the house convention in the code first: an existing schema, a parser, a type. A convention you find beats any default below.

### document
Check. A JSON Schema 2020-12 object that compiles. `$schema` is `https://json-schema.org/draft/2020-12/schema`. Put shared shapes in `$defs` and refer to them with local `$ref`s; none remote.

### descriptions
Check. Every property has a `description` that says what it means and its unit, not its type. Add `examples` for anything a reader would guess at.

### naming
Ask. Suggested: lowerCamelCase property names (AIP-140), one case only; enum values in UPPER_SNAKE_CASE (AIP-126); a timestamp is an RFC 3339 string named `fooTime` (AIP-142); `id` is an opaque string. If the document already exists, keep its case.

### required
Check. `required` names only properties that exist in `properties`. Ask which fields are required. Suggested: require only what a reader cannot work without, and decide up front, because adding a required field later breaks every writer.

### nullable
Ask. Optional (the key may be absent) and null (the key is present with no value) are different states. Suggested: use optional only (AIP-203) and omit the key when there is no value; allow `null` only when "cleared on purpose" must differ from "not set", and then say so in the description. State a default in `default`, never imply it.

### extensibility
Check. Every object says `additionalProperties` (or `unevaluatedProperties`) explicitly. Ask which. Suggested: open (`true`) for documents that other versions write, so readers skip unknown fields; closed (`false`) for config files, where a typo should fail loudly. Closing an open object later breaks writers.

### enums
Ask. A closed `enum` promises that no value is ever added: a reader that switches over it fails on a new value. Suggested: use an enum only for a set that is truly fixed; otherwise a string with `examples`, and document that readers must handle unknown values (AIP-126 reserves an unspecified value for this).

### compatibility
Check. These break callers: removing a property (optional ones too), making a property required, narrowing a type or enum, tightening `minimum`, `maximum`, `minLength`, `maxLength`, `minItems`, `maxItems` or `pattern` (a changed pattern counts), and closing `additionalProperties`. The check follows `properties`, `$defs`, `items` and the like by position; a change inside `allOf`, `anyOf`, `oneOf` or `prefixItems`, or to `const`, `format` or `dependentRequired`, is not checked, so judge those yourself and list them in `compatibility`. Adding an optional property, loosening a constraint and adding a `$defs` entry are safe. Widening an enum or type is safe for writers but can break readers that switch on it. A breaking change names its callers.

### versioning
Ask. Suggested: one schema per contract, compatible changes shipped in place (AIP-180); a breaking change is a new document kind or file name (`foo.v2`), with a stated period when both are read. Add a `version` or `kind` field to the document only when readers must pick a parser by it.

### examples
Ask. Suggested: one minimal valid example and one with every optional field, as `examples` on the root. An example that does not validate is a defect.
