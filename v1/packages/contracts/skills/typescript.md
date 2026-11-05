# TypeScript contract guide

A rule marked check is enforced mechanically; a rule marked ask is a choice for the person, with a suggested default and its trade-off. Use this kind for the surface extensions and core name in code: manifests, context objects, adapters, hooks.

## Intent first

Settle these before drafting: who provides each value (core or an extension), who consumes it, and which direction a value flows, because a change that is safe for one side breaks the other. Read the code that implements and calls the surface first; a convention you find beats any default below.

### document
Check. A `.d.ts` module of `export type`, `export interface`, `export declare function` and `export declare const`. It compiles on its own with `strict` on. It imports only `@earendil-works/*` packages and `node:` built-ins.

### surface
Ask. Suggested: export only what extensions or core must name. A helper type that only one export uses stays unexported. Every export is a promise, and removing one breaks whoever names it.

### tsdoc
Check. Every export has a TSDoc comment. It says what the thing is, who provides it and who consumes it.

### additions
Ask. Suggested: add new fields as optional. A new required field breaks every extension that already provides the value; an optional one breaks nobody.

### unions
Ask. Suggested: a discriminated union with a literal `kind` field over several boolean flags. Adding a member breaks consumers that switch exhaustively, so say whether consumers must handle unknown members.

### types
Check. No `any` in an exported signature and no default export. Use `unknown` and narrow it, or name the type. Ask whether a class belongs in the surface. Suggested: none, unless callers need its behavior; a type or interface describes shape without tying callers to an implementation.

### compatibility
Check. The diff compiles each export from the old and the new version in both directions. If the new type does not fit the old one, values core now produces break old consumers (a widened union, a removed field, a changed type). If the old type does not fit the new one, values extensions already provide break (a new required field, a narrowed return type). A removed export is breaking; a new export or a new optional field is safe. A break names its callers.

### versioning
Ask. Suggested: compatible changes ship in place; a break gets a new export name (`FooV2`) kept beside the old while both are in use.
