import type { Workflow } from '@/workflow/model';

/**
 * buildTestWorkflow returns a one-action definition for the builder's
 * single-action test run. It keeps the workflow name and input declarations so
 * `${{ inputs.* }}` references still resolve, keeps the selected action verbatim
 * with its `depends_on` stripped (the test runs it alone), and drops every
 * other action. The argument is never mutated: the result is a fresh workflow
 * whose one action is a fresh object, so the live canvas keeps its edges.
 */
export function buildTestWorkflow(flow: Workflow, actionName: string): Workflow {
  const action = flow.actions[actionName];
  if (!action) {
    // A caller only tests a selected node, so a missing name is a programming
    // error, not a user path; fail loud rather than emit an empty definition.
    throw new Error(`buildTestWorkflow: no action named ${actionName}`);
  }

  return {
    ...flow,
    actions: { [actionName]: { ...action, dependsOn: [] } },
  };
}
