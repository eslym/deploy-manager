# WebSocket dependency review

Reviewed for issue #72 on 2026-11-19, before adding a dependency.

## Selection

Use `github.com/coder/websocket` rather than implementing RFC 6455 or selecting
the older Gorilla package. Coder maintains the library after adopting it from
nhooyr; the project is active and its v1 API provides context-aware reads and
writes, explicit origin policy, ping/pong controls, and bounded message reads.
These match the endpoint's needs without introducing transitive dependencies.

## Security and license

The upstream project documents Autobahn test-suite conformance. The latest
release inspected was v1.8.15 (2026-06-15); Open Source Insights reported no
direct advisories for that version as of this review. This is not a substitute
for ongoing vulnerability monitoring. The module is ISC-licensed, a permissive
license; retain its license notice in distributed source/binary notices as
required by the project's distribution process.

The library's upgrade origin defaults are not the application's authorization
policy. The application validates exact Origin, canonical Host, HTTPS policy,
and the session cookie before upgrade, and configures the upgrader explicitly.
Application-level queue, frame, rate, liveness, and write bounds remain
mandatory regardless of library guarantees.
