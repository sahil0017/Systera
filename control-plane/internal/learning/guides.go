package learning

// typeGuide is the reusable teaching material for one canonical graph node type (§2.7,
// §8.5). Lessons combine it with what the graph actually says about a component
// (dependencies, interfaces, data ownership, failure modes, ADR alternatives) so the
// explanation is about *this* design, not a generic textbook entry.
type typeGuide struct {
	Purpose      string
	Problem      string
	Alternatives []TradeOff
	Failures     []FailureWalkthrough
	Questions    []InterviewQuestion
	Exercises    []Exercise
}

func q(level, question string, covers ...string) InterviewQuestion {
	return InterviewQuestion{Level: level, Question: question, GoodAnswersCover: covers}
}

var guides = map[string]typeGuide{
	"service": {
		Purpose: "A service is a unit of ownership: it encapsulates a set of business capabilities, the rules that keep them consistent, and the data they need, behind an explicit interface.",
		Problem: "Without a clear owner, business rules get duplicated across callers and every change needs coordination between unrelated parts of the system.",
		Alternatives: []TradeOff{
			{Option: "Module inside a modular monolith", WhenToChoose: "Small-to-medium team, boundaries still evolving, strong transactional needs.", Pros: "In-process calls, one deployment, ACID transactions.", Cons: "Shared release blast radius; boundaries erode without discipline."},
			{Option: "Separate deployable microservice", WhenToChoose: "Independent scaling or release cadence, separate team ownership, isolation requirements.", Pros: "Independent deploy/scale, fault isolation.", Cons: "Network failures, distributed data, higher operational cost."},
			{Option: "Serverless functions", WhenToChoose: "Spiky, event-driven workloads with little shared state.", Pros: "Pay-per-use, no capacity planning.", Cons: "Cold starts, vendor limits, harder local testing."},
		},
		Failures: []FailureWalkthrough{
			{Scenario: "Instance crash or bad deploy", Severity: "HIGH", Detection: "Health checks, error-rate and latency SLO alerts.", Impact: "Requests to this capability fail or time out.", Recovery: "Run multiple stateless replicas behind a load balancer; roll back via progressive (canary) delivery."},
			{Scenario: "Overload from a traffic spike", Severity: "MEDIUM", Detection: "Saturation metrics (CPU, queue depth, p99 latency).", Impact: "Latency climbs, then cascading timeouts in callers.", Recovery: "Autoscaling, admission control / rate limiting, and load shedding of non-critical work."},
		},
		Questions: []InterviewQuestion{
			q("FOUNDATIONAL", "What responsibilities does this service own, and what should it explicitly not own?", "bounded context", "single owner for business rules", "data ownership"),
			q("INTERMEDIATE", "How would you keep this service available during a deploy that changes its API?", "backward-compatible changes", "versioning", "expand/contract", "canary or blue/green"),
			q("ADVANCED", "When would you split this service in two, and what evidence would you want first?", "independent scaling need", "team ownership", "change coupling metrics", "cost of distributed data"),
		},
		Exercises: []Exercise{
			{Title: "Graceful shutdown and health checks", Difficulty: "INTERMEDIATE", Goal: "Make the service safe to deploy and scale without dropping requests.",
				Steps:              []string{"Expose liveness and readiness endpoints that reflect real dependencies.", "On SIGTERM, stop accepting new work, drain in-flight requests, then exit.", "Run two replicas behind a load balancer and redeploy one while load testing."},
				AcceptanceCriteria: []string{"No failed requests during a rolling deploy.", "Readiness fails while the database is unreachable."}},
		},
	},
	"component": {
		Purpose: "A component is an internal building block of a service with a narrow, cohesive responsibility.",
		Problem: "Large services become hard to reason about; components keep internal responsibilities separated and testable.",
		Alternatives: []TradeOff{
			{Option: "Inline the logic in its caller", WhenToChoose: "The logic is tiny and used in one place.", Pros: "Less indirection.", Cons: "Harder to test and reuse; responsibilities blur."},
			{Option: "Promote to its own service", WhenToChoose: "It needs independent scaling, deployment or ownership.", Pros: "Isolation.", Cons: "Network boundary and operational cost."},
		},
		Failures: []FailureWalkthrough{
			{Scenario: "Hidden coupling to another component's internals", Severity: "MEDIUM", Detection: "Changes in one component regularly break another's tests.", Impact: "Slower, riskier changes.", Recovery: "Introduce an explicit interface and enforce module boundaries with architecture tests."},
		},
		Questions: []InterviewQuestion{
			q("FOUNDATIONAL", "What is the single responsibility of this component?", "cohesion", "clear interface"),
			q("INTERMEDIATE", "How would you test this component in isolation?", "dependency injection", "fakes vs mocks", "contract tests"),
		},
		Exercises: []Exercise{
			{Title: "Enforce a module boundary", Difficulty: "FOUNDATIONAL", Goal: "Prevent other code from reaching into this component's internals.",
				Steps:              []string{"Define the component's public interface.", "Move implementation details behind it.", "Add an architecture test that fails on forbidden imports."},
				AcceptanceCriteria: []string{"Only the interface is importable from outside.", "The architecture test runs in CI."}},
		},
	},
	"api": {
		Purpose: "An API is the contract through which clients use a capability without depending on how it is implemented.",
		Problem: "Without a stable contract every internal change breaks clients, and clients cannot evolve independently.",
		Alternatives: []TradeOff{
			{Option: "REST/JSON over HTTP", WhenToChoose: "Public or browser-facing resources, broad tooling.", Pros: "Ubiquitous, cacheable, easy to debug.", Cons: "Chatty for complex reads; weak typing without OpenAPI."},
			{Option: "gRPC", WhenToChoose: "Internal service-to-service calls needing low latency and strict schemas.", Pros: "Typed, efficient, streaming.", Cons: "Less browser-friendly; binary payloads are harder to inspect."},
			{Option: "GraphQL", WhenToChoose: "Many clients with different read shapes over the same data.", Pros: "Flexible reads, fewer round trips.", Cons: "Caching and query-cost control are harder."},
			{Option: "Asynchronous events instead of a request API", WhenToChoose: "The caller does not need an immediate answer.", Pros: "Decoupling and buffering.", Cons: "Eventual consistency; harder to trace."},
		},
		Failures: []FailureWalkthrough{
			{Scenario: "Breaking contract change", Severity: "HIGH", Detection: "Consumer contract tests fail; client error rates rise after release.", Impact: "Clients break.", Recovery: "Version the API, make additive changes only, deprecate with a timeline."},
			{Scenario: "Retry storm on timeouts", Severity: "HIGH", Detection: "Request rate multiplies while success rate drops.", Impact: "Duplicate side effects and overload.", Recovery: "Idempotency keys on mutating endpoints, exponential backoff with jitter, retry budgets."},
			{Scenario: "Abuse or runaway client", Severity: "MEDIUM", Detection: "Per-client request rate anomalies.", Impact: "Noisy-neighbour degradation.", Recovery: "Authentication, per-client rate limits and quotas at the edge."},
		},
		Questions: []InterviewQuestion{
			q("FOUNDATIONAL", "How do you make a POST endpoint on this API safe to retry?", "idempotency key", "deduplication store", "returning the original result"),
			q("INTERMEDIATE", "How would you evolve this API without breaking existing clients?", "additive changes", "versioning", "deprecation policy", "consumer-driven contract tests"),
			q("ADVANCED", "How would you design pagination for a large, changing collection behind this API?", "cursor vs offset", "stable sort keys", "consistency under concurrent writes"),
		},
		Exercises: []Exercise{
			{Title: "Idempotent create endpoint", Difficulty: "INTERMEDIATE", Goal: "Make a mutating endpoint safe under client retries.",
				Steps:              []string{"Accept an Idempotency-Key header.", "Store key, request hash and response in the same transaction as the write.", "Return the stored response for a replay; reject a reused key with a different body."},
				AcceptanceCriteria: []string{"Two identical requests create one record.", "Same key + different body returns 409/422."}},
		},
	},
	"database": {
		Purpose: "A database is the system of record: it stores state durably and enforces the integrity and consistency guarantees the business relies on.",
		Problem: "Application memory is lost on restart and cannot coordinate concurrent writers; a database provides durability, transactions and queryable state.",
		Alternatives: []TradeOff{
			{Option: "Relational (PostgreSQL/MySQL)", WhenToChoose: "Relationships, transactions and ad-hoc queries matter.", Pros: "ACID, constraints, mature tooling.", Cons: "Horizontal write scaling needs sharding or partitioning."},
			{Option: "Key-value / wide-column (DynamoDB, Cassandra)", WhenToChoose: "Known access patterns at very high scale.", Pros: "Predictable latency, horizontal scale.", Cons: "Limited queries; data modelled around access patterns."},
			{Option: "Document store (MongoDB)", WhenToChoose: "Aggregates read and written as a whole with flexible schema.", Pros: "Natural aggregate fit.", Cons: "Cross-document consistency and joins are weaker."},
		},
		Failures: []FailureWalkthrough{
			{Scenario: "Primary failure", Severity: "HIGH", Detection: "Connection errors, replication health checks.", Impact: "Writes fail until failover completes.", Recovery: "Synchronous or semi-synchronous replica with automated failover; clients retry with backoff."},
			{Scenario: "Connection or lock saturation", Severity: "HIGH", Detection: "Pool wait time, lock waits, slow query log.", Impact: "Latency spikes across every dependent service.", Recovery: "Connection pooling, query and index tuning, short transactions, read replicas for read traffic."},
			{Scenario: "Data loss or corruption", Severity: "HIGH", Detection: "Integrity checks, failed restores in drills.", Impact: "Permanent business data loss.", Recovery: "Point-in-time recovery with regularly tested restores."},
		},
		Questions: []InterviewQuestion{
			q("FOUNDATIONAL", "Which entities does this database own, and which constraints protect their integrity?", "primary/foreign keys", "unique constraints", "check constraints"),
			q("INTERMEDIATE", "How would you scale reads and then writes for this database?", "indexes", "read replicas and replication lag", "caching", "partitioning/sharding key choice"),
			q("ADVANCED", "How would you run a zero-downtime schema migration on the largest table?", "expand/contract", "backfill in batches", "online index builds", "dual writes or views"),
		},
		Exercises: []Exercise{
			{Title: "Restore drill", Difficulty: "INTERMEDIATE", Goal: "Prove backups can actually be restored within the recovery objective.",
				Steps:              []string{"Enable point-in-time recovery.", "Restore to a new instance at a chosen timestamp.", "Verify row counts and a checksum of critical tables."},
				AcceptanceCriteria: []string{"Restore completes within the RTO.", "Recovered data matches the chosen point in time (RPO)."}},
		},
	},
	"table": {
		Purpose: "A table models one entity or relationship with the constraints that keep its rows valid.",
		Problem: "Without an explicit schema and constraints, invalid or duplicate data accumulates silently.",
		Alternatives: []TradeOff{
			{Option: "Normalised tables", WhenToChoose: "Write-heavy data with integrity needs.", Pros: "No duplication, strong integrity.", Cons: "Joins on read."},
			{Option: "Denormalised read model", WhenToChoose: "Read-heavy queries with stable shapes.", Pros: "Fast reads.", Cons: "Must be kept in sync; duplication."},
		},
		Failures: []FailureWalkthrough{
			{Scenario: "Missing index on a hot query", Severity: "MEDIUM", Detection: "Sequential scans in slow-query logs.", Impact: "Latency and database CPU spikes.", Recovery: "Add the index concurrently; review query plans in CI for critical queries."},
		},
		Questions: []InterviewQuestion{
			q("FOUNDATIONAL", "What uniquely identifies a row in this table, and what prevents duplicates?", "natural vs surrogate keys", "unique constraints"),
			q("INTERMEDIATE", "Which indexes does this table need for its main access patterns?", "composite index order", "selectivity", "write amplification"),
		},
		Exercises: []Exercise{
			{Title: "Constraint-first schema", Difficulty: "FOUNDATIONAL", Goal: "Push validity rules into the schema.",
				Steps:              []string{"List the invariants for this entity.", "Express each as NOT NULL, UNIQUE, CHECK or FK.", "Write tests that try to violate each invariant."},
				AcceptanceCriteria: []string{"Every invariant is rejected by the database itself."}},
		},
	},
	"cache": {
		Purpose: "A cache keeps frequently read data close to the reader to cut latency and protect the system of record from read load.",
		Problem: "Repeatedly reading the same data from the database wastes capacity and adds latency on hot paths.",
		Alternatives: []TradeOff{
			{Option: "No cache; tune the database", WhenToChoose: "Load is moderate and freshness is critical.", Pros: "No staleness or invalidation logic.", Cons: "Database carries all read load."},
			{Option: "In-process cache", WhenToChoose: "Small, rarely changing data.", Pros: "Fastest, no network.", Cons: "Per-instance copies, inconsistent across replicas."},
			{Option: "CDN / HTTP caching", WhenToChoose: "Public, cacheable responses.", Pros: "Offloads at the edge.", Cons: "Coarse invalidation."},
		},
		Failures: []FailureWalkthrough{
			{Scenario: "Cache stampede after expiry or cache loss", Severity: "HIGH", Detection: "Sudden database load spike when hit ratio drops.", Impact: "Database overload.", Recovery: "Request coalescing (single-flight), jittered TTLs, warm-up, and serving stale while revalidating."},
			{Scenario: "Stale or inconsistent data", Severity: "MEDIUM", Detection: "User-visible mismatches; version checks.", Impact: "Wrong data shown.", Recovery: "Explicit invalidation on write (or write-through) and bounded TTLs."},
		},
		Questions: []InterviewQuestion{
			q("FOUNDATIONAL", "What happens to the system if this cache is empty?", "cold start", "database headroom", "graceful degradation"),
			q("INTERMEDIATE", "Cache-aside, write-through or write-behind: which fits here and why?", "consistency", "write latency", "failure behaviour"),
			q("ADVANCED", "How would you handle a hot key receiving most of the traffic?", "key replication", "local near-cache", "request coalescing"),
		},
		Exercises: []Exercise{
			{Title: "Stampede protection", Difficulty: "INTERMEDIATE", Goal: "Keep the database safe when a popular key expires.",
				Steps:              []string{"Implement cache-aside reads.", "Add single-flight so one request rebuilds a missing key.", "Add TTL jitter and load-test an expiry."},
				AcceptanceCriteria: []string{"One database read per missing key under concurrent load."}},
		},
	},
	"queue": {
		Purpose: "A queue buffers work between producers and consumers so they can run at different speeds and fail independently.",
		Problem: "Doing slow or unreliable work inside a request makes users wait and couples availability to every downstream dependency.",
		Alternatives: []TradeOff{
			{Option: "Synchronous call", WhenToChoose: "The caller needs the result immediately.", Pros: "Simple, immediate consistency.", Cons: "Coupled availability and latency."},
			{Option: "Event stream / log (Kafka, NATS JetStream)", WhenToChoose: "Many consumers or replay is needed.", Pros: "Fan-out, replay, ordering per partition.", Cons: "More operational complexity."},
		},
		Failures: []FailureWalkthrough{
			{Scenario: "Poison message", Severity: "MEDIUM", Detection: "Same message redelivered repeatedly; consumer error spikes.", Impact: "Consumer stalls behind it.", Recovery: "Bounded retries then a dead-letter queue with alerting and a replay tool."},
			{Scenario: "Growing backlog", Severity: "MEDIUM", Detection: "Queue depth and age-of-oldest-message alerts.", Impact: "Work completes late.", Recovery: "Scale consumers, prioritise, shed non-critical work."},
			{Scenario: "Duplicate delivery", Severity: "HIGH", Detection: "Duplicate side effects in audits.", Impact: "Double charges or emails.", Recovery: "Idempotent consumers keyed on message ID."},
		},
		Questions: []InterviewQuestion{
			q("FOUNDATIONAL", "What delivery guarantee does this queue give, and what must consumers do about it?", "at-least-once", "idempotency", "acknowledgements"),
			q("INTERMEDIATE", "How would you handle a message that always fails?", "retry with backoff", "dead-letter queue", "alerting and replay"),
		},
		Exercises: []Exercise{
			{Title: "Idempotent consumer with DLQ", Difficulty: "INTERMEDIATE", Goal: "Process messages exactly once in effect.",
				Steps:              []string{"Record processed message IDs in the same transaction as the side effect.", "Skip already-processed IDs.", "After N failures, move the message to a dead-letter queue."},
				AcceptanceCriteria: []string{"Redelivering a message has no extra effect.", "A poison message ends in the DLQ without blocking others."}},
		},
	},
	"topic": {
		Purpose: "A topic broadcasts domain events so other parts of the system can react without the publisher knowing who they are.",
		Problem: "Direct calls to every interested party couple the publisher to all of them and make adding a new consumer a change to the publisher.",
		Alternatives: []TradeOff{
			{Option: "Direct synchronous calls", WhenToChoose: "Few consumers that must succeed together.", Pros: "Simple, immediate.", Cons: "Tight coupling, cascading failures."},
			{Option: "Work queue (point-to-point)", WhenToChoose: "Exactly one consumer group processes each message.", Pros: "Simpler semantics.", Cons: "No fan-out."},
		},
		Failures: []FailureWalkthrough{
			{Scenario: "Event lost between database commit and publish", Severity: "HIGH", Detection: "Downstream state diverges from the source of truth.", Impact: "Missed reactions (no email, no stock update).", Recovery: "Transactional outbox: write the event in the same transaction, relay it asynchronously."},
			{Scenario: "Incompatible schema change", Severity: "HIGH", Detection: "Consumer deserialisation errors.", Impact: "Consumers stop processing.", Recovery: "Versioned event schemas with compatibility checks in CI."},
			{Scenario: "Out-of-order events", Severity: "MEDIUM", Detection: "State regressions in projections.", Impact: "Wrong derived state.", Recovery: "Partition by entity key and carry versions so consumers ignore stale events."},
		},
		Questions: []InterviewQuestion{
			q("FOUNDATIONAL", "Why publish an event here instead of calling consumers directly?", "decoupling", "fan-out", "independent failure"),
			q("INTERMEDIATE", "How do you guarantee an event is published if and only if the database change commits?", "transactional outbox", "change data capture", "dual-write problem"),
			q("ADVANCED", "How would you evolve this event's schema with consumers you don't control?", "backward/forward compatibility", "schema registry", "upcasting", "versioned topics"),
		},
		Exercises: []Exercise{
			{Title: "Transactional outbox publisher", Difficulty: "INTERMEDIATE", Goal: "Publish events reliably without dual writes.",
				Steps:              []string{"Insert the event into an outbox table in the business transaction.", "A relay reads unpublished rows with FOR UPDATE SKIP LOCKED and publishes them.", "Mark rows published only after broker acknowledgement."},
				AcceptanceCriteria: []string{"Killing the relay mid-batch loses no events.", "Consumers tolerate the duplicates the relay may produce."}},
		},
	},
	"event": {
		Purpose: "An event records a fact that happened in the domain, in a form other components can depend on.",
		Problem: "Without explicit events, other components poll or reach into another service's database to find out what changed.",
		Alternatives: []TradeOff{
			{Option: "Thin event + callback query", WhenToChoose: "Payload is large or sensitive.", Pros: "Small events, no data duplication.", Cons: "Extra round trip; coupling to the source API."},
			{Option: "Event-carried state transfer", WhenToChoose: "Consumers need the data and must stay autonomous.", Pros: "No callback needed.", Cons: "Larger events; data duplicated downstream."},
		},
		Failures: []FailureWalkthrough{
			{Scenario: "Event duplicated or replayed", Severity: "MEDIUM", Detection: "Duplicate effects downstream.", Impact: "Double processing.", Recovery: "Stable event IDs and idempotent consumers."},
		},
		Questions: []InterviewQuestion{
			q("FOUNDATIONAL", "Is this event a fact or a command, and why does the difference matter?", "past tense facts", "ownership", "who decides"),
			q("INTERMEDIATE", "What should this event's payload contain?", "identity and version", "minimal vs full state", "PII considerations"),
		},
		Exercises: []Exercise{
			{Title: "Event contract", Difficulty: "FOUNDATIONAL", Goal: "Define a versioned, testable event schema.",
				Steps:              []string{"Write a JSON Schema for the event with id, type, version, occurred_at and subject.", "Add a compatibility check against the previous version in CI."},
				AcceptanceCriteria: []string{"A breaking field removal fails CI."}},
		},
	},
	"external_dependency": {
		Purpose: "An external dependency is a capability bought rather than built (payments, email, identity, maps).",
		Problem: "Building commodity capabilities in-house costs time and expertise that is better spent on the product's differentiators.",
		Alternatives: []TradeOff{
			{Option: "Build in-house", WhenToChoose: "It is a core differentiator or vendor terms are unacceptable.", Pros: "Full control.", Cons: "Cost, expertise, compliance burden."},
			{Option: "Multiple providers behind an adapter", WhenToChoose: "The dependency is business-critical.", Pros: "Failover, negotiating leverage.", Cons: "Lowest-common-denominator features, more testing."},
		},
		Failures: []FailureWalkthrough{
			{Scenario: "Provider outage or slowdown", Severity: "HIGH", Detection: "Timeout and error rates on outbound calls.", Impact: "Dependent user journeys fail.", Recovery: "Timeouts, circuit breaker, queue and retry later, degrade gracefully or fail over to a secondary provider."},
			{Scenario: "Lost or duplicated callback (webhook)", Severity: "HIGH", Detection: "Reconciliation mismatches.", Impact: "State diverges (e.g. paid but order pending).", Recovery: "Idempotent webhook handling plus periodic reconciliation against the provider."},
		},
		Questions: []InterviewQuestion{
			q("FOUNDATIONAL", "What happens to users when this provider is down?", "graceful degradation", "timeouts", "user messaging"),
			q("ADVANCED", "How would you keep your records consistent with the provider's?", "webhooks", "idempotency", "reconciliation jobs"),
		},
		Exercises: []Exercise{
			{Title: "Circuit breaker and reconciliation", Difficulty: "ADVANCED", Goal: "Survive provider outages without losing consistency.",
				Steps:              []string{"Wrap provider calls with timeouts and a circuit breaker.", "Record intent locally before calling.", "Add a reconciliation job that compares local state with the provider's."},
				AcceptanceCriteria: []string{"Simulated outage does not exhaust threads.", "Reconciliation detects and repairs a dropped webhook."}},
		},
	},
	"deployment_unit": {
		Purpose: "A deployment unit is how code actually runs: the process, container or function, its scaling policy and its release mechanism.",
		Problem: "Logical design says nothing about capacity, isolation or rollout; the deployment unit makes those decisions explicit.",
		Alternatives: []TradeOff{
			{Option: "Containers on a managed orchestrator", WhenToChoose: "Several services, need for consistent rollout and scaling.", Pros: "Portable, autoscaling, rolling deploys.", Cons: "Platform complexity."},
			{Option: "Serverless", WhenToChoose: "Spiky or low traffic, small team.", Pros: "No servers to manage.", Cons: "Cold starts, execution limits."},
			{Option: "VMs", WhenToChoose: "Special hardware or legacy constraints.", Pros: "Full control.", Cons: "Slower scaling, more patching."},
		},
		Failures: []FailureWalkthrough{
			{Scenario: "Zone outage", Severity: "HIGH", Detection: "Zone-level health alarms.", Impact: "Capacity loss.", Recovery: "Spread replicas across zones with headroom to absorb one zone's loss."},
			{Scenario: "Bad release", Severity: "HIGH", Detection: "Canary error budget burn.", Impact: "User-facing errors.", Recovery: "Progressive delivery with automated rollback."},
		},
		Questions: []InterviewQuestion{
			q("INTERMEDIATE", "How many replicas does this need, and across how many zones?", "peak load", "N+1 headroom", "zone failure"),
			q("ADVANCED", "How would you roll out a risky change here safely?", "canary", "feature flags", "automated rollback on SLO burn"),
		},
		Exercises: []Exercise{
			{Title: "Canary with automated rollback", Difficulty: "ADVANCED", Goal: "Limit the blast radius of a bad release.",
				Steps:              []string{"Route a small share of traffic to the new version.", "Compare error rate and latency to the baseline.", "Roll back automatically when the comparison fails."},
				AcceptanceCriteria: []string{"An injected error in the canary triggers rollback before full rollout."}},
		},
	},
	"domain": {
		Purpose: "A domain (bounded context) groups the concepts, language and rules that belong together, so each part of the model means one thing.",
		Problem: "In one big model the same word means different things to different teams, and every change ripples everywhere.",
		Alternatives: []TradeOff{
			{Option: "Fewer, larger contexts", WhenToChoose: "Early product, small team.", Pros: "Less integration.", Cons: "Model ambiguity grows."},
			{Option: "Finer-grained contexts", WhenToChoose: "Distinct teams and language.", Pros: "Clear ownership.", Cons: "More translation between contexts."},
		},
		Failures: []FailureWalkthrough{
			{Scenario: "Boundary drawn in the wrong place", Severity: "MEDIUM", Detection: "Most changes touch two contexts at once.", Impact: "Chatty integration and coordinated releases.", Recovery: "Re-draw the boundary around the change patterns; merge or split contexts."},
		},
		Questions: []InterviewQuestion{
			q("FOUNDATIONAL", "Which concepts belong to this domain, and which are borrowed from others?", "ubiquitous language", "context map"),
			q("ADVANCED", "How does this domain integrate with its neighbours?", "anti-corruption layer", "published language", "events vs calls"),
		},
		Exercises: []Exercise{
			{Title: "Context map", Difficulty: "FOUNDATIONAL", Goal: "Make domain relationships explicit.",
				Steps:              []string{"List this domain's core concepts.", "Draw its upstream/downstream relationships.", "Mark where translation (anti-corruption) is needed."},
				AcceptanceCriteria: []string{"Every cross-domain dependency has an owner and an integration style."}},
		},
	},
	"system": {
		Purpose: "The system node is the boundary of what is being designed: everything inside is owned, everything outside is a dependency or an actor.",
		Problem: "Without an explicit boundary, scope creeps and responsibilities for integrations are unclear.",
		Failures: []FailureWalkthrough{
			{Scenario: "Unclear scope", Severity: "LOW", Detection: "Recurring debates about who owns an integration.", Impact: "Gaps and duplicated work.", Recovery: "Keep a context diagram current and review it with each ADR."},
		},
		Questions: []InterviewQuestion{
			q("FOUNDATIONAL", "Walk through the system end to end for its most important user journey.", "entry point", "services involved", "data written", "async follow-ups"),
			q("ADVANCED", "What are the top three risks in this system and how is each mitigated?", "single points of failure", "data consistency", "security boundary"),
		},
	},
	"actor": {
		Purpose: "An actor is a person or external system that uses the system; it anchors design decisions in real user needs.",
		Problem: "Designing without explicit actors produces capabilities nobody needs and misses the ones they do.",
		Questions: []InterviewQuestion{
			q("FOUNDATIONAL", "What does this actor need to achieve, and which components serve it?", "user journey", "entry points"),
		},
	},
	"region": {
		Purpose: "A region places compute and data geographically for latency, resilience and data-residency requirements.",
		Problem: "A single location is a single point of failure and may violate residency rules or add latency for distant users.",
		Alternatives: []TradeOff{
			{Option: "Single region, multi-zone", WhenToChoose: "Most workloads.", Pros: "Simple, strong consistency.", Cons: "Region outage takes the system down."},
			{Option: "Active-passive multi-region", WhenToChoose: "Strict recovery objectives.", Pros: "Survives region loss.", Cons: "Cost; failover must be rehearsed."},
			{Option: "Active-active multi-region", WhenToChoose: "Global users, very high availability.", Pros: "Low latency, no failover gap.", Cons: "Conflict resolution and much higher complexity."},
		},
		Failures: []FailureWalkthrough{
			{Scenario: "Region outage", Severity: "HIGH", Detection: "Regional health checks.", Impact: "Full outage for a single-region design.", Recovery: "Cross-region replication and a rehearsed failover runbook."},
		},
		Questions: []InterviewQuestion{
			q("ADVANCED", "What are the RPO and RTO for this region failing, and how are they achieved?", "replication mode", "failover automation", "DNS/traffic steering"),
		},
	},
	"secret_boundary": {
		Purpose: "A secret/trust boundary marks where identity, credentials and data sensitivity change, so controls can be applied deliberately.",
		Problem: "Without explicit trust boundaries, sensitive data and credentials leak across components that should never see them.",
		Failures: []FailureWalkthrough{
			{Scenario: "Credential leak", Severity: "HIGH", Detection: "Secret scanning, anomalous access.", Impact: "Unauthorised access.", Recovery: "Short-lived credentials from a secrets manager, rotation, least privilege."},
		},
		Questions: []InterviewQuestion{
			q("INTERMEDIATE", "What crosses this boundary, and how is each crossing authenticated and authorised?", "mTLS / tokens", "least privilege", "audit logging"),
		},
	},
	"slo": {
		Purpose: "An SLO turns 'reliable enough' into a measurable target that guides alerting and the pace of change.",
		Problem: "Without an explicit target, teams either over-invest in reliability or ship changes that silently erode it.",
		Questions: []InterviewQuestion{
			q("INTERMEDIATE", "How would you choose this SLO's target and measurement window?", "user expectations", "SLIs", "error budgets"),
			q("ADVANCED", "What do you do when the error budget is exhausted?", "release freeze", "reliability work", "burn-rate alerts"),
		},
		Exercises: []Exercise{
			{Title: "Burn-rate alert", Difficulty: "INTERMEDIATE", Goal: "Alert on SLO risk, not on every error.",
				Steps:              []string{"Define the SLI as a ratio of good to total events.", "Create fast and slow multi-window burn-rate alerts."},
				AcceptanceCriteria: []string{"A brief blip does not page; a sustained burn does."}},
		},
	},
	"threat": {
		Purpose: "A threat records a credible way the system could be attacked or misused, so mitigations are designed rather than hoped for.",
		Problem: "Security added after the fact misses design-level weaknesses that are expensive to fix later.",
		Questions: []InterviewQuestion{
			q("INTERMEDIATE", "Which STRIDE category does this threat fall in, and which control mitigates it?", "spoofing/tampering/...", "preventive vs detective controls"),
		},
	},
	"workload": {
		Purpose: "A workload captures expected traffic and data growth, which drives capacity, scaling and cost decisions.",
		Problem: "Designs without explicit load assumptions are either over-provisioned or fall over at the first spike.",
		Questions: []InterviewQuestion{
			q("INTERMEDIATE", "Estimate peak requests per second and storage growth for this workload.", "back-of-the-envelope maths", "read/write ratio", "peak vs average"),
		},
	},
	"test": {
		Purpose: "A test node links verification to the design element it protects.",
		Problem: "Without traceable tests, nobody knows which guarantees are actually checked.",
		Questions: []InterviewQuestion{
			q("FOUNDATIONAL", "What failure would this test catch that others would not?", "test pyramid", "contract vs integration tests"),
		},
	},
}

func guideFor(nodeType string) typeGuide {
	if g, ok := guides[nodeType]; ok {
		return g
	}
	return guides["component"]
}
