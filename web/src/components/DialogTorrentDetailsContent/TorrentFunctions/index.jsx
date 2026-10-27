import axios from 'axios'
import { memo, useState } from 'react'
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
  LinearProgress,
} from '@material-ui/core'
import ptt from 'parse-torrent-title'
import { useTranslation } from 'react-i18next'
import { useQueryClient } from 'react-query'

import { SmallLabel, MainSectionButtonGroup, RemoteControls } from './style'
import { SectionSubName } from '../style'

const TorrentFunctions = memo(
  ({
    hash,
    viewedFileList,
    playableFileList,
    name,
    title,
    setViewedFileList,
    remoteDownload,
    remoteState,
    remoteProgress,
    remoteError,
    remoteConfigured,
  }) => {
    const { t } = useTranslation()
    const queryClient = useQueryClient()
    const [isRemotePending, setIsRemotePending] = useState(false)
    const [remoteRequestError, setRemoteRequestError] = useState(null)
    const [isRemoteOffConfirmOpen, setIsRemoteOffConfirmOpen] = useState(false)
    const isRemote = Boolean(remoteDownload)
    const hasRemoteProgress = typeof remoteProgress === 'number'
    const remoteDisabled = isRemotePending || remoteConfigured === undefined || (!isRemote && !remoteConfigured)

    const sendRemote = on => {
      setRemoteRequestError(null)
      setIsRemotePending(true)
      axios
        .post(torrentsHost(), { action: 'remote', hash, remote_download: on })
        .then(() => queryClient.invalidateQueries('torrents', undefined, { cancelRefetch: true }))
        .catch(error =>
          setRemoteRequestError(
            error.response?.status === 409 ? t('Remote.InfoPending') : error.response?.data?.error || error.message,
          ),
        )
        .finally(() => setIsRemotePending(false))
    }
    const onRemoteToggle = ({ target: { checked } }) => (checked ? sendRemote(true) : setIsRemoteOffConfirmOpen(true))
    const closeRemoteOffConfirm = () => setIsRemoteOffConfirmOpen(false)
    const confirmRemoteOff = () => {
      closeRemoteOffConfirm()
      sendRemote(false)
    }

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
        <SmallLabel mb={10}>{t('Remote.Title')}</SmallLabel>
        <RemoteControls>
          <FormControlLabel
            control={
              <Checkbox checked={isRemote} disabled={remoteDisabled} onChange={onRemoteToggle} color='secondary' />
            }
            label={t('Remote.Enable')}
          />
          {isRemote && (
            <div className='remote-status'>
              <span>
                {t(`Remote.State.${remoteState || 'unknown'}`)}
                {hasRemoteProgress && ` ${remoteProgress}%`}
              </span>
              {hasRemoteProgress && (
                <LinearProgress variant='determinate' color='secondary' value={Math.min(remoteProgress, 100)} />
              )}
            </div>
          )}
          {remoteConfigured === false && <FormHelperText>{t('Remote.NotConfigured')}</FormHelperText>}
          {isRemote && remoteError && <FormHelperText error>{remoteError}</FormHelperText>}
          {remoteRequestError && <FormHelperText error>{remoteRequestError}</FormHelperText>}
        </RemoteControls>

        <Dialog open={isRemoteOffConfirmOpen} onClose={closeRemoteOffConfirm}>
          <DialogTitle>{t('Remote.OffConfirmTitle')}</DialogTitle>
          <DialogContent>
            <DialogContentText>{t('Remote.OffConfirmText')}</DialogContentText>
          </DialogContent>
          <DialogActions>
            <Button variant='outlined' onClick={closeRemoteOffConfirm} color='secondary'>
              {t('Cancel')}
            </Button>
            <Button variant='contained' onClick={confirmRemoteOff} color='secondary' autoFocus>
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
)

export default TorrentFunctions
