import { DyoHeading } from '@app/elements/dyo-heading'
import { PatchVersionImage, VersionImage } from '@app/models'
import useTranslation from 'next-translate/useTranslation'
import { VerionState, VersionActions } from './use-version-state'
import VersionViewList from './version-view-list'
import VersionViewTile from './version-view-tile'

interface VersionImagesSectionProps {
  disabled?: boolean
  state: VerionState
  actions: VersionActions
}

const VersionImagesSection = (props: VersionImagesSectionProps) => {
  const { state, actions, disabled } = props
  const { version, viewMode } = state

  const { t } = useTranslation('images')

  return version.images.length ? (
    viewMode === 'tile' ? (
      <VersionViewTile disabled={disabled} state={state} />
    ) : (
      <VersionViewList state={state} actions={actions} />
    )
  ) : (
    <DyoHeading element="h3" className="text-md text-center text-light-eased pt-32">
      {t('noItems')}
    </DyoHeading>
  )
}

export default VersionImagesSection

export const mergeImagePatch = (oldImage: VersionImage, newImage: PatchVersionImage): VersionImage => ({
  ...oldImage,
  ...newImage,
  config: {
    ...oldImage.config,
    ...newImage.config,
  },
})
