import { Link } from 'react-router-dom'
import { useI18n } from '../i18n'
import { featureAnchor, type FeatureKey } from './featureSetup'

interface Props {
  feature: FeatureKey
  titleKey: string
  bodyKey: string
}

// Shown on a page whose feature is off. The sidebar hides such a page, but its
// URL still works for bookmarks and doc links, so it says what the feature does
// and sends the reader to the one place that explains how to turn it on.
export default function FeatureOffNotice({ feature, titleKey, bodyKey }: Props) {
  const { t } = useI18n()
  return (
    <div className="mb-6 p-5 rounded-xl bg-indigo-50 dark:bg-indigo-900/20 border border-indigo-200 dark:border-indigo-800">
      <h3 className="text-sm font-semibold text-indigo-900 dark:text-indigo-300 mb-2">{t(titleKey)}</h3>
      <p className="text-sm text-indigo-700 dark:text-indigo-400 mb-3">{t(bodyKey)}</p>
      <Link
        to={`/settings#${featureAnchor(feature)}`}
        className="text-sm font-medium text-indigo-700 dark:text-indigo-300 underline underline-offset-2 hover:text-indigo-900 dark:hover:text-indigo-200"
      >
        {t('feature.howToEnable')}
      </Link>
    </div>
  )
}
