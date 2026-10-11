# AI policy

## In the product

composewright can ask a language model to fill gaps in a deployment plan. The model is optional (`llm.provider: none` or `--no-llm` turns it off), never sees secrets, has no tools and no network access of its own, returns only a plan in a fixed schema, cannot change images, needs or policy results from the deterministic rules, and its answer passes the same policy gates as any other plan. Every value it writes is marked as model-written. Nothing it proposes runs without your approval.

## In contributions

AI-assisted contributions are welcome under the same rules as any other:

- The person who opens the pull request has read, understood and tested every line, and answers review questions about it.
- Code passes `make lint test build`; behaviour changes come with tests.
- Do not paste secrets, private repositories or other people's code into a model to produce a contribution.
- Generated documentation is checked against the code it describes.
