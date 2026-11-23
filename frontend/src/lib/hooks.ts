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
  type AuthStatus,
  type BrowseResponse,
  type CreateSyncJobBody,
  type Health,
  type LibrarySearchResponse,
  type QueueResponse,
  type QueueTransition,
  type SettingsEnvelope,
  type SyncJob,
  type SyncJobsResponse,
  type User,
  type VaultResponse,
} from './api'

export const queryKeys = {
  authStatus: ['auth', 'status'] as const,
  me: ['auth', 'me'] as const,
  settings: ['settings'] as const,
  health: ['health'] as const,
  queue: ['queue'] as const,
  vault: ['vault'] as const,
  syncJobs: ['sync', 'jobs'] as const,
  syncJob: (id: number) => ['sync', 'jobs', id] as const,
  browse: (path: string) => ['browse', path] as const,
  librarySearch: (q: string) => ['library', 'search', q] as const,
  tokens: ['auth', 'tokens'] as const,
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

export function useSyncJobs(): UseQueryResult<SyncJobsResponse> {
  return useQuery({
    queryKey: queryKeys.syncJobs,
    queryFn: api.syncJobs,
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
}

export function useApproveQueueItem(): UseMutationResult<
  QueueTransition,
  Error,
  ApproveQueueVariables
> {
  const client = useQueryClient()
  return useMutation({
    mutationFn: ({ id, libraryCacheId }) => api.approveQueueItem(id, libraryCacheId),
    onSuccess: () => {
      void client.invalidateQueries({ queryKey: queryKeys.queue })
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
