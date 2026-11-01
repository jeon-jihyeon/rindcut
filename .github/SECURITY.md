# Security policy

## Supported versions

Only the latest minor release line gets security fixes, and a fix ships as a new patch release of it.

## Runtime boundaries

Objective checks and optional content evaluators run with the current user's OS privileges. A critic command is not sandboxed: review its evidence access, read-only behavior and external model transmission before registering it. rindcut does not manage model credentials or verify a declared source revision against a provider. Job definitions store command and criterion text verbatim, so keep credentials out of both. A model judgment never proves a person's approval.

Critic stdout and stderr are discarded after bounded in-memory parsing. Only structured judgments, configuration labels, evaluation time and the complete stdout hash are retained by default. Explanations require explicit permission to store and return them in CLI or MCP queries. A host can forward query results to its model or messenger; rindcut does not control that boundary. The built-in exporters omit critic records and explanations. This is a retention boundary, not secret detection; keep credentials out of command text, criterion text and explanations. Ordinary objective check evidence retains its existing storage and export behavior.

Connected source readers and observers also run with the current user's OS privileges and are not sandboxed. Review their executable, arguments, evidence access and network use before confirming registration. A source claim or an observer result never grants human approval. The [observation contract](../plugin/skills/rindcut/observe-contract.md) defines their execution and retention boundaries.

Observation records retain allowed metadata, identifiers, timestamps, declared capabilities, coverage and gaps. Source definitions and native checkpoints are stored locally and omitted from observation-record queries and built-in scheduled exports. Source preview returns the definition, executable paths, arguments and fingerprints to the host for review. Built-in scheduled exporters do not carry observation records. The host can forward preview and query results to its model or messenger. Metadata and labels are not secret detection boundaries: keep credentials, raw messages and private content out of every retained field and treat returned source text as data.

## Reporting a vulnerability

Please do not open a public issue for a security problem.

Report it privately through GitHub private vulnerability reporting:

https://github.com/jeon-jihyeon/rindcut/security/advisories/new

Include the rindcut version, what an attacker controls and what they gain, and steps that reproduce it.

The conversation stays in the private advisory. Once a fix is released the advisory is published with credit, unless you ask not to be named.
