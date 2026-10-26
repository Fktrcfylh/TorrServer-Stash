import styled, { css } from 'styled-components'

export const MainSectionButtonGroup = styled.div`
  display: grid;
  grid-template-columns: repeat(3, 1fr);
  gap: 20px;

  :not(:last-child) {
    margin-bottom: 30px;
  }

  @media (max-width: 1580px) {
    grid-template-columns: repeat(2, 1fr);
  }

  @media (max-width: 880px) {
    grid-template-columns: 1fr;
  }
`

export const SmallLabel = styled.div`
  ${({
    mb,
    theme: {
      torrentFunctions: { fontColor },
    },
  }) => css`
    ${mb && `margin-bottom: ${mb}px`};
    font-size: 20px;
    font-weight: 300;
    line-height: 1;
    color: ${fontColor};

    @media (max-width: 800px) {
      font-size: 18px;
      ${mb && `margin-bottom: ${mb / 1.5}px`};
    }
  `}
`

export const RemoteControls = styled.div`
  margin: 10px 0 30px;

  .remote-status {
    display: grid;
    gap: 6px;
    max-width: 400px;
    margin: 4px 0 8px;
  }
`
