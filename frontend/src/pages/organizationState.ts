// Eligibility only; the server still verifies saved proof, physical objects,
// original-source preservation, target absence and executor isolation.
export function canAbandonOrganizationJob(job: { state: string; items: Array<{ state: string }> }): boolean {
  return job.state === "abandoning" || (job.state === "needs_review"
    && job.items.some((item) => item.state === "prepared" || item.state === "staging")
    && job.items.every((item) => item.state === "planned" || item.state === "prepared" || item.state === "staging"));
}
