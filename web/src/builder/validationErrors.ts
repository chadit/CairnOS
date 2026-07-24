import type { ApiError } from '@/api/types';

/**
 * actionFromDetails pulls an action name out of a validation error's details.
 * The engine's structured details vary by code; an "action" key is the common
 * carrier, with "name" as a fallback. When absent, the error stays
 * workflow-level and has no node to mark.
 */
function actionFromDetails(details: ApiError['details']): string | null {
  if (!details) {
    return null;
  }
  const value = details.action ?? details.name;
  return typeof value === 'string' ? value : null;
}

/**
 * errorCodesByAction groups canonical validation error codes by the action
 * they name, the shape the builder store's setErrorCodes consumes to mark
 * nodes. Errors that name no action are omitted; the caller counts them
 * separately for a workflow-level note.
 */
export function errorCodesByAction(errors: ApiError[] | undefined): Record<string, string[]> {
  const byAction: Record<string, string[]> = {};
  for (const issue of errors ?? []) {
    const action = actionFromDetails(issue.details);
    if (action) {
      byAction[action] = [...(byAction[action] ?? []), issue.code];
    }
  }
  return byAction;
}
