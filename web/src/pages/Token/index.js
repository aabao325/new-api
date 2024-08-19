import React from 'react';
import TokensTable from '../../components/TokensTable';
import { Banner, Layout } from '@douyinfe/semi-ui';
const Token = () => (
  <>
    <Layout>
      <Layout.Header>
        <Banner
          type="success"
          description='点击“聊天”直接访问ChatGPT Web进行对话。'
        />
      </Layout.Header>
      <Layout.Content>
        <TokensTable />
      </Layout.Content>
    </Layout>
  </>
);

export default Token;
