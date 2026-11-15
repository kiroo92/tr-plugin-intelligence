import { definePlugin } from '@tokenflux/tokenrouter-plugin-sdk'
import IntelligencePage from './IntelligencePage.vue'
import './style.css'
function register(): void { definePlugin({ pages: { home: IntelligencePage, admin: IntelligencePage } }) }
if (import.meta.env.DEV) void import('./dev').then(register)
else register()
