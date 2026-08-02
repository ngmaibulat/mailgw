'use strict';
module.exports = {
  async up(queryInterface, Sequelize) {
    await queryInterface.createTable('Transaction', {
      
      id: {
        allowNull: false,
        autoIncrement: true,
        primaryKey: true,
        type: Sequelize.INTEGER
      },

      dt: {
        type: Sequelize.DATE
      },

      uuid: {
        type: Sequelize.STRING
      },

      action: {
        type: Sequelize.STRING
      },

      encoding: {
        type: Sequelize.STRING
      },

      sender: {
        type: Sequelize.STRING
      },

      rcpt_list: {
        type: Sequelize.STRING
      },

      rcpt_count_accept: {
        type: Sequelize.INTEGER
      },

      rcpt_count_tempfail: {
        type: Sequelize.INTEGER
      },

      rcpt_count_reject: {
        type: Sequelize.INTEGER
      },

      delay_data_post: {
        type: Sequelize.DOUBLE
      },

      data_bytes: {
        type: Sequelize.INTEGER
      },

      mime_part_count: {
        type: Sequelize.INTEGER
      },

      createdAt: {
        allowNull: false,
        type: Sequelize.DATE
      },

      updatedAt: {
        allowNull: false,
        type: Sequelize.DATE
      }
    });
  },
  async down(queryInterface, Sequelize) {
    await queryInterface.dropTable('Transaction');
  }
};
