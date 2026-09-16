# Patterns no number catches

The ast-grep half of verbosity is 137 handcrafted rules - taste, encoded - and they are not published. These ten families come from the anti-slop prompt the same study wrote, which is the closest published statement of what it treats as condensable; they are not a listing of the rule corpus. Each is a smell, not a defect: the exception column is where the pattern is the right call, and a finding raised against it there is wrong.

| Pattern | What it looks like | Replace with | Not a finding when |
|---|---|---|---|
| **Defensive check that cannot fire** | `if x is None` on a value the only caller just constructed; a `try`/`catch` whose handler returns the same thing the call would have | Delete it, or move the check to the boundary where untrusted input actually arrives | It sits at a trust boundary - user input, a network reply, a parsed file, a plugin's return |
| **Cast to silence the type checker** | `as any`, `interface{}`, `# type: ignore`, an unchecked downcast | Fix the type that forced it; if it cannot be fixed, one comment saying which external contract is wrong | The interop really is untyped (reflection, a dynamic plugin registry, a JSON blob whose shape is data) and the cast is checked |
| **Single-use variable** | A value assigned to a name and read once, immediately below | Inline it | The name is the explanation - a bare literal or a long expression at the call site would need a comment instead |
| **Trivial wrapper** | A function whose body is one call, forwarding its arguments unchanged | Call the inner function | It is an interface implementation, a seam a test substitutes, or a documented stable name over a moving one |
| **Comment narrating the next line** | `// increment the counter` above `count++`; a docstring restating the signature | Delete it. Keep comments for the non-obvious why, constraint, or ceiling | The API is public and the docstring is its contract |
| **If/else ladder** | Four or more branches comparing the same value | A table, a map of handlers, a `switch`, or polymorphism | The branches are genuinely unrelated conditions, not one value's cases |
| **Heavy nesting** | Three or more levels of indentation inside one function | Early return for the guard cases; extract the innermost block | The nesting mirrors a nested data structure being walked |
| **A pile of helpers** | Several one-caller private functions added alongside the feature, each used once | Inline the ones that exist only to name a line; keep the ones that name a concept | Each helper is independently testable and is actually tested |
| **God function or class** | A function gathering unrelated responsibilities, growing a branch per feature | Split by responsibility; the new branch usually wants its own function | Nothing - this one has no benign form. It is also what the concentration number measures |
| **Files grouped by layer, not by feature** | `handlers.py`, `helpers.py`, `utils.py` accumulating unrelated functions | Group by what the code is about, so one change touches one file | The project's stated convention is layered, and you are matching it |

## Using this list

Two rules keep it from becoming a style war.

**One finding per pattern, with the replacement named.** "This is verbose" is not actionable; "these three blocks differ only in the status code - take it as a parameter" is. If you cannot name the replacement, you have not found anything yet.

**The project's linter and style guide outrank this file.** Anything the linter already enforces is the linter's job. Where the project has settled a convention, follow it rather than this table, and never raise one of these as a blocking objection - they are suggestions with a reason attached.
