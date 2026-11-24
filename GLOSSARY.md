# Model router

The mesh that places a request on a node which can run the named model.

## Language

**Decision model**:
A model that reads a state and named, typed questions in one forward pass and returns a probability per option instead of generated text; a stanza serves one as `api_type: decision`.
_Avoid_: classifier, judge, chat model

**Headroom**:
Memory on each GPU that admission will not book.
_Avoid_: reservation, reserve, overhead, margin

**Job**:
A long-running generation a backend runs after answering its submit; the router routes its follow-up calls by job id and keeps its model loaded until it ends.
_Avoid_: task, async request

**Reservation**:
VRAM this node has booked for a model that is starting or loaded.
_Avoid_: headroom, overhead, margin

**Start cooldown**:
The period after a model fails to become healthy during which that node will not start that model again.
_Avoid_: backoff, penalty, circuit breaker
