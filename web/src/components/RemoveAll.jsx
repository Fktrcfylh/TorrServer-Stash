import { Button, Dialog, DialogActions, DialogContent, DialogContentText, DialogTitle } from '@material-ui/core'
import ListItem from '@material-ui/core/ListItem'
import ListItemIcon from '@material-ui/core/ListItemIcon'
import ListItemText from '@material-ui/core/ListItemText'
import DeleteIcon from '@material-ui/icons/Delete'
import { useState } from 'react'
import { torrentsHost } from 'utils/Hosts'
import { useTranslation } from 'react-i18next'

import UnsafeButton from './UnsafeButton'

// Resolves to null on success, else to the server error text (wipe fails when NAS-flagged torrents cannot be removed)
const fnRemoveAll = () =>
  fetch(torrentsHost(), {
    method: 'post',
    body: JSON.stringify({ action: 'wipe' }),
    headers: {
      Accept: 'application/json, text/plain, */*',
      'Content-Type': 'application/json',
    },
  })
    .then(response =>
      response.ok
        ? null
        : response
            .json()
            .then(body => body?.error || response.statusText)
            .catch(() => response.statusText || String(response.status)),
    )
    .catch(error => error.message)

export default function RemoveAll({ isOffline, isLoading }) {
  const { t } = useTranslation()
  const [open, setOpen] = useState(false)
  const [isPending, setIsPending] = useState(false)
  const [error, setError] = useState(null)
  const closeDialog = () => setOpen(false)
  const openDialog = () => {
    setError(null)
    setOpen(true)
  }
  const removeAll = () => {
    // UnsafeButton reads `disabled` only on mount, so a repeated click is ignored here
    if (isPending) return
    setError(null)
    setIsPending(true)
    fnRemoveAll().then(failure => {
      setIsPending(false)
      if (failure) setError(failure)
      else closeDialog()
    })
  }

  return (
    <>
      <ListItem disabled={isOffline || isLoading} button key={t('RemoveAll')} onClick={openDialog}>
        <ListItemIcon>
          <DeleteIcon />
        </ListItemIcon>

        <ListItemText primary={t('RemoveAll')} />
      </ListItem>

      <Dialog open={open} onClose={() => !isPending && closeDialog()}>
        <DialogTitle>{t('DeleteTorrents?')}</DialogTitle>
        {error && (
          <DialogContent>
            <DialogContentText color='error'>{error}</DialogContentText>
          </DialogContent>
        )}
        <DialogActions>
          <Button variant='outlined' onClick={closeDialog} disabled={isPending} color='secondary'>
            {t('Cancel')}
          </Button>

          <UnsafeButton
            timeout={5}
            startIcon={<DeleteIcon />}
            variant='contained'
            onClick={removeAll}
            color='secondary'
            autoFocus
          >
            {t('OK')}
          </UnsafeButton>
        </DialogActions>
      </Dialog>
    </>
  )
}
