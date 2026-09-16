# Comment Authoring References

Load this reference for language-specific public API conventions or source attribution. The skill's one-or-two-line limit applies to local comments, not public API contracts.

## Language Conventions

| Language | Convention | Primary source |
| --- | --- | --- |
| Go | `// Name ...` directly above the declaration, with no separating blank line | [Go doc comments](https://go.dev/doc/comment) |
| Python | Triple-quoted docstring; one-line summary, then a blank line before detail | [PEP 257](https://peps.python.org/pep-0257/) |
| Rust | `///` for item docs and `//!` for enclosing-item docs | [The rustdoc book](https://doc.rust-lang.org/rustdoc/) |
| Java | `/** ... */` Javadoc attached to the API; document parameters, returns, and thrown errors when the signature does not suffice | [Google Java Style Guide](https://google.github.io/styleguide/javaguide.html) |
| C++ | Follow the repository's Doxygen or `///` convention consistently | [Google C++ Style Guide](https://google.github.io/styleguide/cppguide.html) |
| JavaScript / TypeScript | Follow the repository's JSDoc or TSDoc convention; document runtime behavior rather than repeating static types | [TypeScript JSDoc reference](https://www.typescriptlang.org/docs/handbook/jsdoc-supported-types.html) |

## Research Basis

- [Pascarella and Bacchelli, *Classifying Code Comments in Java Open-Source Software Systems*](https://doi.org/10.1109/MSR.2017.63) (MSR 2017): manual classification of 2,000 files found summary comments outnumber rationale comments by roughly twenty to one.
- [Wen, Nagy, Bavota and Lanza, *A Large-Scale Empirical Study on Code-Comment Inconsistencies*](https://doi.org/10.1109/ICPC.2019.00019) (ICPC 2019): comments drifting out of step with the code they describe is the dominant real-world failure, not comment scarcity.
- [Martin Fowler et al., *Refactoring*](https://refactoring.com/book/): improve unclear code before explaining it with comments.
- [Steve McConnell, *Code Complete*](https://www.oreilly.com/library/view/code-complete-2nd/0672322409/): document intent and constraints rather than mechanics.
- [Google C++ Style Guide: Comments](https://google.github.io/styleguide/cppguide.html): comments must remain accurate and describe non-obvious behavior and interfaces.
- [Google Java Style Guide](https://google.github.io/styleguide/javaguide.html): Javadoc placement and formatting conventions.
- [Go documentation comments](https://go.dev/doc/comment): canonical godoc syntax and package/declaration guidance.
- [PEP 257](https://peps.python.org/pep-0257/): canonical Python docstring conventions.
- [The rustdoc book](https://doc.rust-lang.org/rustdoc/): canonical Rust documentation syntax.

## Practical References

- [Google Testing Blog: To Comment or Not to Comment?](https://testing.googleblog.com/2017/07/code-health-to-comment-or-not-to-comment.html)
- [Google Engineering Practices: What to Look for in a Code Review](https://google.github.io/eng-practices/review/reviewer/looking-for.html)
- [Jane Street: 10 Tips for Writing Comments](https://blog.janestreet.com/10-tips-for-writing-comments-plus-one-more/)
- [Kevlin Henney: Comment Only What the Code Cannot Say](https://accu.org/journals/overload/28/157/henney_2796/)
