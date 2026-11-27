import {
  useMutation,
  useQuery,
  useQueryClient,
  type UseMutationResult,
  type UseQueryResult,
} from '@tanstack/react-query'
import { useEffect, useState } from 'react'
import {
  api,
  type ApiTokenCreated,
  type ApiTokensResponse,
  type ArrConnection,
  type ArrConnectionKind,
  type ArrConnectionPayload,
  type ArrConnectionsResponse,
  type ArrConnectionTestPayload,
  type ArrConnectionTestResult,
  type AuthStatus,
  type BrowseResponse,
  type CreateSyncJobBody,
  type Health,
  type LibrarySearchResponse,
  type LogLevel,
  type LogsQuery,
  type LogsResponse,
  type QueueApproveResponse,
  type QueueIdentifyResponse,
  type QueueMatchResponse,
  type QueueResponse,
  type QueueTransition,
  type SettingsEnvelope,
  type SyncJob,
  type SyncJobStatus,
  type SyncJobsResponse,
  type User,
  type VaultApplyResponse,
  type VaultOrganiseRunsResponse,
  type VaultOrganiseScope,
  type VaultPlanResponse,
  type VaultResponse,
} from './api'

export const queryKeys = {
  authStatus: ['auth', 'status'] as const,
  me: ['auth', 'me'] as const,
  settings: ['settings'] as const,
  health: ['health'] as const,
  queue: ['queue'] as const,
  vault: ['vault'] as const,
  organiseRuns: ['vault', 'organise', 'runs'] as const,
  syncJobs: ['sync', 'jobs'] as const,
  /**
   * One job list, keyed by its status filter. The bare `syncJobs` key stays the
   * invalidation prefix, so a write drops every filter and the detail at once.
   */
  syncJobsList: (status?: SyncJobStatus) => ['sync', 'jobs', 'list', status ?? 'all'] as const,
  syncJob: (id: number) => ['sync', 'jobs', id] as const,
  browse: (path: string) => ['browse', path] as const,
  librarySearch: (q: string) => ['library', 'search', q] as const,
  tokens: ['auth', 'tokens'] as const,
  arrConnections: ['settings', 'arr'] as const,
  logs: (query: LogsQuery = {}) => ['logs', query] as const,
  logLevel: ['logs', 'level'] as const,
}

/** Public first-run probe; drives the route gate. */
export function useAuthStatus(): UseQueryResult<AuthStatus> {
  return useQuery({
    queryKey: queryKeys.authStatus,
    queryFn: api.authStatus,
    retry: false,
    staleTime: 5_000,
  })
}

export function useMe(enabled = true): UseQueryResult<User> {
  return useQuery({
    queryKey: queryKeys.me,
    queryFn: api.me,
    enabled,
    retry: false,
  })
}

export function useSettings(enabled = true): UseQueryResult<SettingsEnvelope> {
  return useQuery({
    queryKey: queryKeys.settings,
    queryFn: api.settings,
    enabled,
  })
}

/**
 * Saved Radarr/Sonarr connections. Never returns plaintext keys: the API only
 * exposes the masked suffix, and the query is invalidated after every write.
 */
export function useArrConnections(enabled = true): UseQueryResult<ArrConnectionsResponse> {
  return useQuery({
    queryKey: queryKeys.arrConnections,
    queryFn: api.arrConnections,
    enabled,
    retry: false,
  })
}

export function useSaveArrConnection(): UseMutationResult<
  ArrConnection,
  Error,
  { kind: ArrConnectionKind; payload: ArrConnectionPayload }
> {
  const client = useQueryClient()
  return useMutation({
    mutationFn: ({ kind, payload }) => api.saveArrConnection(kind, payload),
    onSuccess: () => {
      void client.invalidateQueries({ queryKey: queryKeys.arrConnections })
    },
  })
}

export function useDeleteArrConnection(): UseMutationResult<void, Error, ArrConnectionKind> {
  const client = useQueryClient()
  return useMutation({
    mutationFn: (kind) => api.deleteArrConnection(kind),
    onSuccess: () => {
      void client.invalidateQueries({ queryKey: queryKeys.arrConnections })
    },
  })
}

/**
 * Variables for the connection test: a bare kind (test the saved connection) or
 * an object carrying the draft values typed into the form. The request never
 * persists anything, so no list invalidation is needed.
 */
export type ArrTestConnectionVariables =
  | ArrConnectionKind
  | { kind: ArrConnectionKind; payload?: ArrConnectionTestPayload }

export function useTestArrConnection(): UseMutationResult<
  ArrConnectionTestResult,
  Error,
  ArrTestConnectionVariables
> {
  return useMutation({
    mutationFn: (variables) =>
      typeof variables === 'string'
        ? api.testArrConnection(variables)
        : api.testArrConnection(variables.kind, variables.payload),
  })
}

export function useHealth(): UseQueryResult<Health> {
  return useQuery({
    queryKey: queryKeys.health,
    queryFn: api.health,
    staleTime: 10_000,
  })
}

export function useQueue(): UseQueryResult<QueueResponse> {
  return useQuery({
    queryKey: queryKeys.queue,
    queryFn: api.queue,
    staleTime: 5_000,
  })
}

export function useVault(): UseQueryResult<VaultResponse> {
  return useQuery({
    queryKey: queryKeys.vault,
    queryFn: api.vault,
    staleTime: 10_000,
  })
}

/** Audit history of applied rollbacks and renames (#47), newest first. */
export function useOrganiseRuns(): UseQueryResult<VaultOrganiseRunsResponse> {
  return useQuery({
    queryKey: queryKeys.organiseRuns,
    queryFn: () => api.vaultOrganiseRuns(),
    staleTime: 10_000,
    retry: false,
  })
}

/** Plans a rename; writes nothing, so a press is always safe to repeat. */
export function useOrganisePreview(): UseMutationResult<
  VaultPlanResponse,
  Error,
  VaultOrganiseScope
> {
  return useMutation({
    mutationFn: (scope) => api.vaultOrganisePreview(scope),
  })
}

/**
 * Applies a previewed plan. A success makes both the vault listing and the
 * history stale: the paths changed and a new audit run exists.
 */
export function useOrganiseApply(): UseMutationResult<VaultApplyResponse, Error, string> {
  const client = useQueryClient()
  return useMutation({
    mutationFn: (planId) => api.vaultOrganiseApply(planId),
    onSuccess: () => {
      void client.invalidateQueries({ queryKey: queryKeys.vault })
      void client.invalidateQueries({ queryKey: queryKeys.organiseRuns })
    },
  })
}

/**
 * The job list, optionally narrowed to one public status. `held` is the
 * server-side filter (`?status=held`) for jobs that exist but wait (#48).
 */
export function useSyncJobs(status?: SyncJobStatus): UseQueryResult<SyncJobsResponse> {
  return useQuery({
    queryKey: queryKeys.syncJobsList(status),
    queryFn: () => api.syncJobs(status),
    staleTime: 5_000,
  })
}

/**
 * One job detail. Polls while the job is queued/running so the Sync Studio
 * flips to the report without a manual refresh; a terminal status stops it.
 */
export function useSyncJob(id: number | null): UseQueryResult<SyncJob> {
  return useQuery({
    queryKey: queryKeys.syncJob(id ?? 0),
    queryFn: () => api.syncJob(id as number),
    enabled: id !== null,
    refetchInterval: (query) => {
      const status = query.state.data?.status
      return status === 'queued' || status === 'running' ? 1_500 : false
    },
  })
}

export function useCreateSyncJob(): UseMutationResult<SyncJob, Error, CreateSyncJobBody> {
  const client = useQueryClient()
  return useMutation({
    mutationFn: (body) => api.createSyncJob(body),
    onSuccess: (job) => {
      client.setQueryData(queryKeys.syncJob(job.id), job)
      void client.invalidateQueries({ queryKey: queryKeys.syncJobs })
    },
  })
}

export type ApproveQueueVariables = {
  id: number
  /** Manual arr library match for a queue row the automatic parse could not identify. */
  libraryCacheId?: number
  /** "Start now": skip the tag gate for this one job (setting-gated server-side). */
  forceIgnoreTagFilter?: boolean
}

export function useApproveQueueItem(): UseMutationResult<
  QueueApproveResponse,
  Error,
  ApproveQueueVariables
> {
  const client = useQueryClient()
  return useMutation({
    mutationFn: ({ id, libraryCacheId, forceIgnoreTagFilter }) =>
      api.approveQueueItem(id, { libraryCacheId, forceIgnoreTagFilter }),
    onSuccess: () => {
      // No job id comes back any more: the row leaves the pending list and the
      // job is born during extraction, so the list is refreshed and the Studio's
      // cache is dropped rather than holding a stale "no job yet" read.
      void client.invalidateQueries({ queryKey: queryKeys.queue })
      void client.invalidateQueries({ queryKey: queryKeys.syncJobs })
    },
  })
}

export function useRejectQueueItem(): UseMutationResult<QueueTransition, Error, number> {
  const client = useQueryClient()
  return useMutation({
    mutationFn: (id) => api.rejectQueueItem(id),
    onSuccess: () => {
      void client.invalidateQueries({ queryKey: queryKeys.queue })
    },
  })
}

export type IdentifyQueueVariables = {
  id: number
  /** The arr library the operator picked after a 409 ambiguous_kind. */
  kind?: string
}

/**
 * Identifies one unmatched release with arr's own parser. Deliberately not
 * cached: the operator decides when to ask, and the verdict is not a match
 * until they pick a candidate.
 */
export function useIdentifyQueueItem(): UseMutationResult<
  QueueIdentifyResponse,
  Error,
  IdentifyQueueVariables
> {
  return useMutation({
    mutationFn: ({ id, kind }) => api.identifyQueueItem(id, { kind }),
  })
}

export type MatchQueueVariables = {
  id: number
  libraryCacheId: number
  /** The candidate's identify score, used only for the stored confidence label. */
  confidence?: number
}

/** Persists the operator's candidate pick and refreshes the card it belongs to. */
export function useMatchQueueItem(): UseMutationResult<
  QueueMatchResponse,
  Error,
  MatchQueueVariables
> {
  const client = useQueryClient()
  return useMutation({
    mutationFn: ({ id, libraryCacheId, confidence }) =>
      api.matchQueueItem(id, libraryCacheId, confidence),
    onSuccess: () => {
      void client.invalidateQueries({ queryKey: queryKeys.queue })
    },
  })
}

export function useTokens(): UseQueryResult<ApiTokensResponse> {
  return useQuery({
    queryKey: queryKeys.tokens,
    queryFn: api.tokens,
  })
}

export function useCreateToken(): UseMutationResult<ApiTokenCreated, Error, string> {
  const client = useQueryClient()
  return useMutation({
    mutationFn: (name) => api.createToken(name),
    onSuccess: () => {
      void client.invalidateQueries({ queryKey: queryKeys.tokens })
    },
  })
}

export function useRevokeToken(): UseMutationResult<void, Error, number> {
  const client = useQueryClient()
  return useMutation({
    mutationFn: (id) => api.revokeToken(id),
    onSuccess: () => {
      void client.invalidateQueries({ queryKey: queryKeys.tokens })
    },
  })
}

/** Debounces a fast-changing value (search input) before it drives a query. */
export function useDebouncedValue<T>(value: T, delayMs = 300): T {
  const [debounced, setDebounced] = useState(value)
  useEffect(() => {
    const timer = setTimeout(() => setDebounced(value), delayMs)
    return () => clearTimeout(timer)
  }, [value, delayMs])
  return debounced
}

/** Manual library lookup; only runs once the debounced query has 2+ chars. */
export function useLibrarySearch(q: string): UseQueryResult<LibrarySearchResponse> {
  const query = q.trim()
  return useQuery({
    queryKey: queryKeys.librarySearch(query),
    queryFn: () => api.searchLibrary(query),
    enabled: query.length >= 2,
    staleTime: 10_000,
  })
}

export function useBrowse(path: string | null): UseQueryResult<BrowseResponse> {
  return useQuery({
    queryKey: queryKeys.browse(path ?? ''),
    queryFn: () => api.browse(path ?? '/'),
    enabled: path !== null,
    retry: false,
  })
}

export function useSetup(): UseMutationResult<User, Error, { username: string; password: string }> {
  const client = useQueryClient()
  return useMutation({
    mutationFn: ({ username, password }) => api.setup(username, password),
    onSuccess: () => {
      void client.invalidateQueries({ queryKey: queryKeys.authStatus })
      void client.invalidateQueries({ queryKey: queryKeys.me })
    },
  })
}

export function useLogin(): UseMutationResult<User, Error, { username: string; password: string }> {
  const client = useQueryClient()
  return useMutation({
    mutationFn: ({ username, password }) => api.login(username, password),
    onSuccess: () => {
      void client.invalidateQueries({ queryKey: queryKeys.authStatus })
      void client.invalidateQueries({ queryKey: queryKeys.me })
    },
  })
}

export function useLogout(): UseMutationResult<void, Error, void> {
  const client = useQueryClient()
  return useMutation({
    mutationFn: () => api.logout(),
    onSettled: () => {
      client.clear()
    },
  })
}

/**
 * Changes the signed-in user's password. The server keeps this session valid
 * and revokes the user's other sessions, so there is nothing to redirect to.
 */
export function useChangePassword(): UseMutationResult<
  void,
  Error,
  { currentPassword: string; newPassword: string }
> {
  return useMutation({
    mutationFn: ({ currentPassword, newPassword }) => api.changePassword(currentPassword, newPassword),
  })
}

/**
 * Ring-buffer entries for the Logs page. Polls while `live` is true so the
 * tail follows the appliance without an SSE endpoint.
 */
export function useLogs(query: LogsQuery = {}, live = true): UseQueryResult<LogsResponse> {
  return useQuery({
    queryKey: queryKeys.logs(query),
    queryFn: () => api.logs(query),
    retry: false,
    refetchInterval: live ? 2_000 : false,
  })
}

/** Runtime minimum severity reported by GET /api/logs/level. */
export function useLogLevel(): UseQueryResult<{ level: LogLevel }> {
  return useQuery({
    queryKey: queryKeys.logLevel,
    queryFn: api.logLevel,
    retry: false,
  })
}

export function useSetLogLevel(): UseMutationResult<{ level: LogLevel }, Error, LogLevel> {
  const client = useQueryClient()
  return useMutation({
    mutationFn: (level) => api.setLogLevel(level),
    onSuccess: (data) => {
      client.setQueryData(queryKeys.logLevel, data)
      void client.invalidateQueries({ queryKey: ['logs'] })
    },
  })
}

export function useSaveSettings(): UseMutationResult<
  SettingsEnvelope,
  Error,
  Record<string, unknown>
> {
  const client = useQueryClient()
  return useMutation({
    mutationFn: (settings) => api.saveSettings(settings),
    onSuccess: (data) => {
      client.setQueryData(queryKeys.settings, data)
    },
  })
}
