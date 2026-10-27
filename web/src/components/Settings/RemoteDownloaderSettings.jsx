import axios from 'axios'
import { useState } from 'react'
import { useTranslation } from 'react-i18next'
import { Button, FormGroup, FormHelperText, InputLabel, Select, TextField } from '@material-ui/core'
import { settingsHost } from 'utils/Hosts'

import { SecondarySettingsContent, SettingSectionLabel, SettingsStatusMessage } from './style'

// per type: URL placeholder and the locale keys whose wording differs
const remoteTypes = [
  {
    value: 'transmission',
    label: 'Transmission',
    placeholder: 'http://nas:9091/transmission/rpc',
    urlHint: 'Remote.URLHint',
    testOk: 'Remote.TestOk',
    lpdOff: 'Remote.LpdOff',
  },
  {
    value: 'qbittorrent',
    label: 'qBittorrent',
    placeholder: 'http://nas:8080',
    urlHint: 'Remote.URLHintQbittorrent',
    testOk: 'Remote.TestOkQbittorrent',
    lpdOff: 'Remote.LpdOffQbittorrent',
  },
]

export default function RemoteDownloaderSettings({ settings, updateSettings }) {
  const { t } = useTranslation()
  const [isTesting, setIsTesting] = useState(false)
  // { config, result, error } of the last test; shown only while the form still holds that config
  const [test, setTest] = useState(null)

  const { RemoteDownloader } = settings || {}
  const { Type = 'transmission', URL = '', Username = '', Password = '', DownloadDir = '' } = RemoteDownloader || {}
  const config = { Type, URL, Username, Password, DownloadDir }
  const typeInfo = remoteTypes.find(({ value }) => value === Type) || remoteTypes[0]
  const configKey = JSON.stringify(config)
  const shownTest = test?.config === configKey ? test : null
  const testResult = shownTest?.result
  const testError = shownTest?.error

  const handleChange = (field, value) => {
    updateSettings({
      RemoteDownloader: {
        ...RemoteDownloader,
        [field]: value,
      },
    })
  }

  const testConnection = () => {
    setIsTesting(true)
    setTest(null)
    axios
      .post(settingsHost(), { action: 'test_remote', remote_downloader: config })
      .then(({ data }) => setTest({ config: configKey, result: data }))
      .catch(error => setTest({ config: configKey, error: error.response?.data?.error || error.message }))
      .finally(() => setIsTesting(false))
  }

  return (
    <SecondarySettingsContent>
      <SettingSectionLabel>
        {t('Remote.Settings')}
        <small>{t('Remote.SettingsHint')}</small>
      </SettingSectionLabel>

      <FormGroup style={{ marginBottom: 20 }}>
        <InputLabel htmlFor='RemoteDownloaderType'>{t('Remote.Type')}</InputLabel>
        <Select
          native
          id='RemoteDownloaderType'
          value={Type}
          onChange={e => handleChange('Type', e.target.value)}
          variant='outlined'
          margin='dense'
        >
          {remoteTypes.map(({ value, label }) => (
            <option key={value} value={value}>
              {label}
            </option>
          ))}
        </Select>
      </FormGroup>

      <TextField
        label={t('Remote.URL')}
        helperText={t(typeInfo.urlHint)}
        value={URL}
        onChange={e => handleChange('URL', e.target.value)}
        placeholder={typeInfo.placeholder}
        type='url'
        variant='outlined'
        size='small'
        fullWidth
        margin='normal'
      />

      <TextField
        label={t('Remote.Username')}
        value={Username}
        onChange={e => handleChange('Username', e.target.value)}
        autoComplete='off'
        variant='outlined'
        size='small'
        fullWidth
        margin='normal'
      />

      <TextField
        label={t('Remote.Password')}
        value={Password}
        onChange={e => handleChange('Password', e.target.value)}
        type='password'
        autoComplete='new-password'
        variant='outlined'
        size='small'
        fullWidth
        margin='normal'
      />

      <TextField
        label={t('Remote.DownloadDir')}
        helperText={t('Remote.DownloadDirHint')}
        value={DownloadDir}
        onChange={e => handleChange('DownloadDir', e.target.value)}
        variant='outlined'
        size='small'
        fullWidth
        margin='normal'
      />

      <FormGroup style={{ marginTop: 20 }}>
        <Button variant='outlined' color='secondary' disabled={isTesting} onClick={testConnection}>
          {t('Remote.Test')}
        </Button>
        <FormHelperText>{t('Remote.TestHint')}</FormHelperText>
      </FormGroup>

      {testResult && (
        <>
          <SettingsStatusMessage severity='success'>
            {t(typeInfo.testOk, {
              version: testResult.version,
              rpcVersion: testResult.rpc_version,
              peerPort: testResult.peer_port,
            })}
          </SettingsStatusMessage>
          <SettingsStatusMessage severity={testResult.lpd_enabled ? 'info' : 'warning'}>
            {testResult.lpd_enabled ? t('Remote.LpdOn') : t(typeInfo.lpdOff)}
          </SettingsStatusMessage>
        </>
      )}
      {testError && <SettingsStatusMessage severity='error'>{testError}</SettingsStatusMessage>}
      <br />
    </SecondarySettingsContent>
  )
}
