## Summary
AIP-158 recommends a default `page_size` of 50 and a maximum of 1000 for list methods; when a client requests a value above the maximum, the server should coerce it down to the maximum rather than returning an error.

## Assumptions
- The question refers to the canonical guidance in AIP-158 and its companion AIP-132 (Standard methods: List), both published on google.aip.dev.
- The "default" of 50 and "maximum" of 1000 appear in the AIPs' example proto comments. The normative text says the API "chooses an appropriate default" and "should document the maximum allowed value," so individual APIs may differ, but the AIPs' examples and canonical guidance use 50 and 1000.

## Findings

- **Default page size.** AIP-158 states: "If the user does not specify page_size (or specifies 0), the API chooses an appropriate default, which the API should document. The API must not return an error." [s1] The example message in both AIP-158 and AIP-132 uses a default of 50: "If unspecified, at most 50 books will be returned." [s1][s3]

- **Maximum page size.** The example proto comment in both AIPs specifies a maximum of 1000: "The maximum value is 1000; values above 1000 will be coerced to 1000." [s1][s3] AIP-132 adds that "The comment above the page_size field should document the maximum allowed value, as well as the default value if the field is omitted (or set to 0)." [s3]

- **Server behavior when the client exceeds the maximum.** AIP-158 says: "If the user specifies page_size greater than the maximum permitted by the API, the API should coerce down to the maximum permitted page size." [s1] AIP-132 echoes this: "If a user provides a value greater than the maximum allowed value, the API should coerce the value to the maximum allowed." [s3] The server does **not** return an INVALID_ARGUMENT error in this case; it simply caps the value at the maximum. (By contrast, a negative page_size must produce an INVALID_ARGUMENT error.) [s1][s3]

- **Open debate on the coerce-vs-error behavior.** A 2024 GitHub issue (aip-dev/google.aip.dev #1428) proposed changing the guidance so that exceeding the maximum should return `INVALID_ARGUMENT` instead of silently coercing. The issue remains open and the AIP text has not been changed; the current approved guidance is still to coerce. [s2]
