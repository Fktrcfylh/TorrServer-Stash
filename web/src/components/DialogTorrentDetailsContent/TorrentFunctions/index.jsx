import axios from 'axios'
import { memo, useRef, useState } from 'react'
import { playlistTorrHost, torrentsHost, viewedHost } from 'utils/Hosts'
import { CopyToClipboard } from 'react-copy-to-clipboard'
import {
  Button,
  Checkbox,
  Dialog,
  DialogActions,
  DialogContent,
  DialogContentText,
  DialogTitle,
  FormControlLabel,
  FormHelperText,
  Radio,
  RadioGroup,
  TextField,
} from '@material-ui/core'
import ptt from 'parse-torrent-title'
import { useTranslation } from 'react-i18next'
import { useQueryClient } from 'react-query'

import { SmallLabel, MainSectionButtonGroup, PinControls } from './style'
import { SectionSubName } from '../style'

const fallbackPinNext = 3

const TorrentFunctions = memo(
  ({
    hash,
    viewedFileList,
    playableFileList,
    name,
    title,
    setViewedFileList,
    pinMode,
    pinNext,
    pinProgress,
    pinError,
    useDisk,
    defaultPinNext,
  }) => {
    const { t } = useTranslation()
    const queryClient = useQueryClient()
    const isPinned = pinMode === 'all' || pinMode === 'next'
    const defaultNext = String(defaultPinNext > 0 ? defaultPinNext : fallbackPinNext)
    // Drafts hold only what the user changed; shown values fall back to the server status
    const [draftMode, setDraftMode] = useState('next')
    const [draftNext, setDraftNext] = useState(null)
    const [isPending, setIsPending] = useState(false)
    const [requestError, setRequestError] = useState('')
    const [isOffConfirmOpen, setIsOffConfirmOpen] = useState(false)
    const pinControlsRef = useRef(null)

    const mode = isPinned ? pinMode : draftMode
    const nextValue = draftNext ?? (isPinned ? String(pinNext || 0) : defaultNext)
    const nextValid = /^\d+$/.test(nextValue)
    const settingsLoaded = useDisk !== undefined
    const diskOff = useDisk === false

    const sendPin = (newMode, next) => {
      setRequestError('')
      setIsPending(true)
      return axios
        .post(torrentsHost(), { action: 'pin', hash, pin_mode: newMode, pin_next: next })
        .then(() => {
          if (newMode === 'off') setDraftMode('next')
          // a list poll started before the write would bring back the old pin
          return queryClient.invalidateQueries('torrents', undefined, { cancelRefetch: true })
        })
        .catch(error => setRequestError(error.response?.data?.error || error.message))
        .finally(() => {
          setDraftNext(null)
          setIsPending(false)
        })
    }

    const onPinToggle = ({ target: { checked } }) => {
      if (!checked) {
        setIsOffConfirmOpen(true)
        return
      }
      sendPin(draftMode, nextValid ? Number(nextValue) : Number(defaultNext))
    }

    const confirmOff = () => {
      setIsOffConfirmOpen(false)
      sendPin('off', pinNext || 0)
    }

    const onModeChange = ({ target: { value } }) => {
      if (isPinned) sendPin(value, nextValid ? Number(nextValue) : pinNext || 0)
      else setDraftMode(value)
    }

    const commitNext = event => {
      // a click on another pin control sends the draft N itself
      if (event?.relatedTarget && pinControlsRef.current?.contains(event.relatedTarget)) return
      if (isPending || !isPinned || mode !== 'next' || draftNext === null) return
      if (!nextValid || Number(nextValue) === (pinNext || 0)) {
        setDraftNext(null)
        return
      }
      sendPin('next', Number(nextValue))
    }

    const checkboxDisabled =
      isPending || !settingsLoaded || (diskOff && !isPinned) || (!isPinned && !nextValid && mode === 'next')
    const modeDisabled = isPending || !settingsLoaded || diskOff
    const nextDisabled = modeDisabled || mode !== 'next'

    const latestViewedFileId = viewedFileList?.[viewedFileList?.length - 1]
    const latestViewedFile = playableFileList?.find(({ id }) => id === latestViewedFileId)?.path
    const isOnlyOnePlayableFile = playableFileList?.length === 1
    const latestViewedFileData = latestViewedFile && ptt.parse(latestViewedFile)
    const dropTorrent = () => axios.post(torrentsHost(), { action: 'drop', hash })
    const removeTorrentViews = () =>
      axios.post(viewedHost(), { action: 'rem', hash, file_index: -1 }).then(() => setViewedFileList())
    const fullPlaylistLink = `${playlistTorrHost()}/${encodeURIComponent(name || title || 'file')}.m3u?link=${hash}&m3u`
    const partialPlaylistLink = `${fullPlaylistLink}&fromlast`
    const magnet = `magnet:?xt=urn:btih:${hash}&dn=${encodeURIComponent(name || title)}`

    return (
      <>
        {!isOnlyOnePlayableFile && !!viewedFileList?.length && (
          <>
            <SmallLabel>{t('DownloadPlaylist')}</SmallLabel>
            <SectionSubName mb={10}>
              {t('LatestFilePlayed')}{' '}
              <strong>
                {latestViewedFileData?.title}.
                {latestViewedFileData?.season && (
                  <>
                    {' '}
                    {t('Season')}: {latestViewedFileData?.season}. {t('Episode')}: {latestViewedFileData?.episode}.
                  </>
                )}
              </strong>
            </SectionSubName>

            <MainSectionButtonGroup>
              <a style={{ textDecoration: 'none' }} href={fullPlaylistLink}>
                <Button style={{ width: '100%' }} variant='contained' color='primary' size='large'>
                  {t('Full')}
                </Button>
              </a>

              <a style={{ textDecoration: 'none' }} href={partialPlaylistLink}>
                <Button style={{ width: '100%' }} variant='contained' color='primary' size='large'>
                  {t('FromLatestFile')}
                </Button>
              </a>
            </MainSectionButtonGroup>
          </>
        )}
        <SmallLabel mb={10}>
          {t('Pin.Title')}
          {isPinned && ` ${pinProgress || 0}%`}
        </SmallLabel>
        <PinControls ref={pinControlsRef}>
          <div className='pin-row'>
            <FormControlLabel
              control={
                <Checkbox checked={isPinned} onChange={onPinToggle} disabled={checkboxDisabled} color='secondary' />
              }
              label={t('Pin.Enable')}
            />
            <RadioGroup row value={mode} onChange={onModeChange}>
              <FormControlLabel
                value='all'
                control={<Radio color='secondary' disabled={modeDisabled} />}
                label={t('Pin.ModeAll')}
              />
              <FormControlLabel
                value='next'
                control={<Radio color='secondary' disabled={modeDisabled} />}
                label={t('Pin.ModeNext')}
              />
            </RadioGroup>
            <TextField
              id={`pin-next-${hash}`}
              className='pin-next'
              label={t('Pin.NextCount')}
              type='number'
              size='small'
              variant='outlined'
              value={nextValue}
              error={!nextValid}
              disabled={nextDisabled}
              inputProps={{ min: 0, step: 1 }}
              onChange={({ target: { value } }) => setDraftNext(value)}
              onBlur={commitNext}
              onKeyDown={event => event.key === 'Enter' && commitNext()}
            />
          </div>
          {diskOff && <FormHelperText>{t('Pin.DiskRequired')}</FormHelperText>}
          {pinError === 'no_space' && <FormHelperText error>{t('Pin.NoSpace')}</FormHelperText>}
          {requestError && <FormHelperText error>{requestError}</FormHelperText>}
        </PinControls>

        <Dialog open={isOffConfirmOpen} onClose={() => setIsOffConfirmOpen(false)}>
          <DialogTitle>{t('Pin.OffConfirmTitle')}</DialogTitle>
          <DialogContent>
            <DialogContentText>{t('Pin.OffConfirmText')}</DialogContentText>
          </DialogContent>
          <DialogActions>
            <Button variant='outlined' onClick={() => setIsOffConfirmOpen(false)} color='secondary'>
              {t('Cancel')}
            </Button>
            <Button variant='contained' onClick={confirmOff} color='secondary' autoFocus>
              {t('OK')}
            </Button>
          </DialogActions>
        </Dialog>

        <SmallLabel mb={10}>{t('TorrentState')}</SmallLabel>
        <MainSectionButtonGroup>
          <Button onClick={() => removeTorrentViews()} variant='contained' color='primary' size='large'>
            {t('RemoveViews')}
          </Button>
          <Button onClick={() => dropTorrent()} variant='contained' color='primary' size='large'>
            {t('DropTorrent')}
          </Button>
        </MainSectionButtonGroup>
        <SmallLabel mb={10}>{t('Info')}</SmallLabel>
        <MainSectionButtonGroup>
          {(isOnlyOnePlayableFile || !viewedFileList?.length) && (
            <a style={{ textDecoration: 'none' }} href={fullPlaylistLink}>
              <Button style={{ width: '100%' }} variant='contained' color='primary' size='large'>
                {t('DownloadPlaylist')}
              </Button>
            </a>
          )}
          <CopyToClipboard text={magnet}>
            <Button variant='contained' color='primary' size='large'>
              {t('CopyHash')}
            </Button>
          </CopyToClipboard>
        </MainSectionButtonGroup>
      </>
    )
  },
  // Only pin inputs trigger a re-render; other props are picked up on that render
  (prev, next) =>
    prev.pinMode === next.pinMode &&
    prev.pinNext === next.pinNext &&
    prev.pinProgress === next.pinProgress &&
    prev.pinError === next.pinError &&
    prev.useDisk === next.useDisk &&
    prev.defaultPinNext === next.defaultPinNext,
)

export default TorrentFunctions
