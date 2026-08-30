/**
 * Types both front-ends share with the Go server.
 *
 * Deliberately types only: this module is erased at compile time, so neither
 * bundle gains a chunk or a round trip from importing it. Shared *runtime*
 * helpers would cost the waiting page an extra request during exactly the
 * spike it exists to absorb, which is not a trade worth making to save a line.
 */

/** Where a scheduled room is in its timetable; mirrors queue.Phase in Go. */
export type Phase = "queueing" | "before" | "draw" | "closed";
